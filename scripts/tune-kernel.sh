#!/usr/bin/env bash
# PiQuant — Low-Latency Linux Kernel & Hardware Tuning Script
# Designed for Raspberry Pi 5 (ARM64 BCM2712) & Low-Latency Linux Deployments
#
# Optimizations applied:
# 1. Real-Time Scheduler limits & POSIX FIFO capabilities (SCHED_FIFO, LimitRTPRIO=99)
# 2. Kernel task scheduler bypass via CPU Core Isolation (isolcpus=2,3, nohz_full=2,3)
# 3. Kernel timer tick suppression (full tickless kernel on cores 2,3)
# 4. Hardware IRQ interrupt steering away from quant trading cores (irqaffinity=0,1)
# 5. Socket low-latency network busy-polling & enlarged buffer queues (net.core.busy_poll)
# 6. Lock CPU frequency governor to 'performance' (2.4 GHz Cortex-A76 fixed lock)
# 7. Memory locking (mlockall unlimited) & swappiness suppression (vm.swappiness=1)
# 8. Optional PREEMPT_RT (Real-Time Linux Kernel) installation for microsecond deterministic latency

set -euo pipefail

MODE="${1:-apply}" # apply | check | revert | rt-kernel

echo "================================================================="
echo "   PiQuant Low-Latency Linux Kernel & Hardware Optimizer"
echo "================================================================="

# Check root permissions
if [[ $EUID -ne 0 ]]; then
   echo "Error: This script modifies kernel parameters and must be run as root." >&2
   echo "Please run: sudo ./scripts/tune-kernel.sh [apply|check|revert|rt-kernel]" >&2
   exit 1
fi

ARCH=$(uname -m)
IS_RPI=false
if grep -qi "raspberry" /proc/cpuinfo 2>/dev/null || [[ -d /sys/firmware/devicetree/base/model ]] && grep -qi "raspberry" /sys/firmware/devicetree/base/model 2>/dev/null; then
  IS_RPI=true
fi

# Locate cmdline.txt for Raspberry Pi OS (Bookworm uses /boot/firmware/cmdline.txt)
CMDLINE_PATH="/boot/firmware/cmdline.txt"
if [[ ! -f "$CMDLINE_PATH" && -f "/boot/cmdline.txt" ]]; then
  CMDLINE_PATH="/boot/cmdline.txt"
fi

CONFIG_TXT_PATH="/boot/firmware/config.txt"
if [[ ! -f "$CONFIG_TXT_PATH" && -f "/boot/config.txt" ]]; then
  CONFIG_TXT_PATH="/boot/config.txt"
fi

check_status() {
  echo ""
  echo "--- 1. Linux Kernel & Architecture ---"
  uname -a
  if [[ -f /proc/sys/kernel/sched_rt_runtime_us ]]; then
    echo "  kernel.sched_rt_runtime_us = $(cat /proc/sys/kernel/sched_rt_runtime_us) (-1 means 100% RT CPU budget)"
  fi

  echo ""
  echo "--- 2. CPU Frequency Governor ---"
  for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    if [[ -f "$g" ]]; then
      cpu_num=$(echo "$g" | grep -o "cpu[0-9]*")
      cur_gov=$(cat "$g")
      cur_freq=$(cat "${g/scaling_governor/scaling_cur_freq}" 2>/dev/null || echo "N/A")
      echo "  $cpu_num: governor=$cur_gov (${cur_freq} kHz)"
    fi
  done

  echo ""
  echo "--- 3. Kernel Core Isolation & Tickless State ---"
  if [[ -f /sys/devices/system/cpu/isolated ]]; then
    echo "  Isolated Cores (/sys/devices/system/cpu/isolated): $(cat /sys/devices/system/cpu/isolated 2>/dev/null || echo 'none')"
  fi
  if [[ -f /sys/devices/system/cpu/nohz_full ]]; then
    echo "  Tickless Cores (/sys/devices/system/cpu/nohz_full): $(cat /sys/devices/system/cpu/nohz_full 2>/dev/null || echo 'none')"
  fi
  echo "  Current /proc/cmdline: $(cat /proc/cmdline)"

  echo ""
  echo "--- 4. Network Low-Latency & Busy-Polling ---"
  sysctl net.core.busy_poll net.core.busy_read net.core.rmem_max vm.swappiness 2>/dev/null || true

  echo ""
  echo "--- 5. Real-Time Security Limits (/etc/security/limits.d/) ---"
  if [[ -f /etc/security/limits.d/99-piquant-rt.conf ]]; then
    cat /etc/security/limits.d/99-piquant-rt.conf
  else
    echo "  Not installed. Standard limits apply."
  fi
  echo "================================================================="
}

apply_sysctl() {
  echo "[+] Configuring low-latency sysctl parameters in /etc/sysctl.d/99-piquant-rt.conf..."
  cat <<'EOF' > /etc/sysctl.d/99-piquant-rt.conf
# PiQuant Low-Latency Kernel Tuning
# 1. Network stack zero-latency socket busy-polling (bypasses softirq context switch)
net.core.busy_poll = 50
net.core.busy_read = 50

# 2. Enlarged socket buffer queues (prevents queue drop during order flow bursts)
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.rmem_default = 2097152
net.core.wmem_default = 2097152
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216

# 3. Disable delayed ACKs and optimize connection setup
net.ipv4.tcp_low_latency = 1
net.ipv4.tcp_fastopen = 3
net.core.netdev_max_backlog = 10000

# 4. Memory management & Swappiness (prevent trading memory eviction)
vm.swappiness = 1
vm.dirty_background_ratio = 5
vm.dirty_ratio = 10

# 5. Real-time CPU budget (allow real-time FIFO threads 100% CPU on isolated cores)
kernel.sched_rt_runtime_us = -1
EOF

  sysctl --system >/dev/null
  echo "    sysctl parameters applied successfully."
}

apply_limits() {
  echo "[+] Setting real-time security limits in /etc/security/limits.d/99-piquant-rt.conf..."
  cat <<'EOF' > /etc/security/limits.d/99-piquant-rt.conf
# PiQuant Real-Time & Memlock Limits
piquant          soft    rtprio          99
piquant          hard    rtprio          99
piquant          soft    memlock         unlimited
piquant          hard    memlock         unlimited
piquant          soft    nice            -20
piquant          hard    nice            -20
EOF
  echo "    Limits configured: rtprio 99, memlock unlimited."
}

apply_governor() {
  echo "[+] Locking CPU frequency governor to 'performance'..."
  # Set immediate
  for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    if [[ -f "$g" ]]; then
      echo "performance" > "$g" 2>/dev/null || true
    fi
  done

  # Create persistent systemd service
  cat <<'EOF' > /etc/systemd/system/piquant-governor.service
[Unit]
Description=PiQuant CPU Performance Governor Lock
After=sysinit.target local-fs.target
DefaultDependencies=no

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do [ -f "$g" ] && echo performance > "$g"; done'
RemainAfterExit=yes

[Install]
WantedBy=basic.target
EOF

  systemctl daemon-reload
  systemctl enable piquant-governor.service
  systemctl start piquant-governor.service
  echo "    CPU governor locked to 'performance' on all cores permanently."
}

apply_cmdline_isolation() {
  if [[ ! -f "$CMDLINE_PATH" ]]; then
    echo "[!] cmdline.txt not found at $CMDLINE_PATH (skip on generic x86 dev host)."
    echo "    On Raspberry Pi 5, add these parameters to /boot/firmware/cmdline.txt:"
    echo "    isolcpus=2,3 nohz_full=2,3 rcu_nocbs=2,3 irqaffinity=0,1 transparent_hugepage=always audit=0 nosoftlockup"
    return
  fi

  echo "[+] Configuring Raspberry Pi 5 kernel boot parameters in $CMDLINE_PATH..."
  # Backup if not already backed up
  if [[ ! -f "${CMDLINE_PATH}.orig" ]]; then
    cp "$CMDLINE_PATH" "${CMDLINE_PATH}.orig"
    echo "    Created backup: ${CMDLINE_PATH}.orig"
  fi

  CURRENT_CMDLINE=$(cat "$CMDLINE_PATH" | tr -d '\n')
  PARAMS="isolcpus=2,3 nohz_full=2,3 rcu_nocbs=2,3 irqaffinity=0,1 transparent_hugepage=always audit=0 nosoftlockup"

  # Check if already present
  if grep -q "isolcpus=2,3" "$CMDLINE_PATH"; then
    echo "    cmdline.txt already contains isolcpus=2,3."
  else
    echo -n "$CURRENT_CMDLINE $PARAMS" > "$CMDLINE_PATH"
    echo "    Added to $CMDLINE_PATH:"
    echo "      isolcpus=2,3   -> Bypasses Linux scheduler; Cores 2 & 3 dedicated exclusively to PiQuant"
    echo "      nohz_full=2,3  -> Disables 1000Hz OS kernel timer ticks on Cores 2 & 3"
    echo "      rcu_nocbs=2,3  -> Offloads kernel RCU callbacks to housekeeping Cores 0 & 1"
    echo "      irqaffinity=0,1-> Reroutes all hardware interrupts (Ethernet/USB) to Cores 0 & 1"
    echo "      transparent_hugepage=always -> ARM64 2MB hugepages for orderbook depth tables"
    echo "    [NOTE] A system reboot is required for kernel cmdline parameter changes to take effect."
  fi
}

install_rt_kernel() {
  echo "[+] Checking for Raspberry Pi OS Real-Time (PREEMPT_RT) Kernel..."
  if ! $IS_RPI; then
    echo "    [Notice] Current host is not a Raspberry Pi. PREEMPT_RT kernel packages are architecture-specific."
    echo "    To install PREEMPT_RT on Debian/Ubuntu x86_64: sudo apt install linux-image-rt-amd64"
    return
  fi

  echo "    Updating apt package repositories..."
  apt update
  if apt-cache search linux-image-rt-arm64 | grep -q "linux-image-rt-arm64"; then
    echo "    Installing Raspberry Pi OS 64-bit PREEMPT_RT Real-Time Kernel (linux-image-rt-arm64)..."
    apt install -y linux-image-rt-arm64
    echo "    PREEMPT_RT Kernel installed. Reboot to boot into the real-time kernel."
  else
    echo "    linux-image-rt-arm64 not in default apt repos. You can compile the official Raspberry Pi RT kernel via:"
    echo "    https://www.raspberrypi.com/documentation/computers/linux_kernel.html"
  fi
}

revert_tuning() {
  echo "[-] Reverting kernel and hardware tuning..."
  rm -f /etc/sysctl.d/99-piquant-rt.conf
  rm -f /etc/security/limits.d/99-piquant-rt.conf
  systemctl disable --now piquant-governor.service 2>/dev/null || true
  rm -f /etc/systemd/system/piquant-governor.service
  systemctl daemon-reload

  if [[ -f "${CMDLINE_PATH}.orig" ]]; then
    mv "${CMDLINE_PATH}.orig" "$CMDLINE_PATH"
    echo "    Restored original $CMDLINE_PATH from backup."
  fi
  sysctl --system >/dev/null
  echo "    Reversion complete. Reboot required to clear kernel boot parameters."
}

case "$MODE" in
  check|--check|-c)
    check_status
    ;;
  apply|--apply|-a)
    apply_sysctl
    apply_limits
    apply_governor
    apply_cmdline_isolation
    echo ""
    echo "=== Kernel Tuning Successfully Applied ==="
    echo "Run './scripts/tune-kernel.sh check' to inspect active state."
    ;;
  rt-kernel|--rt-kernel|-rt)
    install_rt_kernel
    ;;
  revert|--revert|-r)
    revert_tuning
    ;;
  *)
    echo "Usage: sudo $0 {apply|check|revert|rt-kernel}"
    exit 1
    ;;
esac
