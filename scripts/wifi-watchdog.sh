#!/usr/bin/env bash
# ==============================================================================
# PiQuant Wi-Fi Health Watchdog & Auto-Reconnect Daemon
# ==============================================================================
# Prevents Raspberry Pi Wi-Fi chip power-saving sleep, continuously monitors
# connectivity, auto-reconnects on drop, and switches to backup mobile hotspot
# if home Wi-Fi drops out.
# ==============================================================================
set -u

LOGFILE="/var/log/piquant-wifi.log"
CONFIG_FILE="/etc/piquant/backup-wifi.conf"
INTERFACE="${1:-wlan0}"
FAIL_COUNT=0
MAX_RETRIES_BEFORE_BOUNCE=4

log() {
    local msg="[$(date '+%Y-%m-%d %H:%M:%S')] $1"
    echo "$msg"
    echo "$msg" >> "$LOGFILE" 2>/dev/null || true
}

# 1. Ensure Wi-Fi hardware power saving is strictly disabled
disable_power_save() {
    if command -v iw &>/dev/null; then
        iw dev "$INTERFACE" set power_save off 2>/dev/null || true
    fi
}

disable_power_save
log "Starting PiQuant Wi-Fi Watchdog on interface: $INTERFACE (Power save: OFF)"

while true; do
    sleep 15
    disable_power_save

    # Find default gateway
    GATEWAY=$(ip route show default 2>/dev/null | awk '/default/ {print $3}' | head -n 1)
    TARGET=${GATEWAY:-"1.1.1.1"}

    # Ping test (2 probes, 2 second timeout)
    if ping -q -c 2 -W 2 "$TARGET" >/dev/null 2>&1 || ping -q -c 2 -W 2 "8.8.8.8" >/dev/null 2>&1; then
        if [ $FAIL_COUNT -gt 0 ]; then
            log "✔ Internet connection RESTORED after $FAIL_COUNT failed cycles. WebSocket feed reconnecting."
            FAIL_COUNT=0
        fi
        continue
    fi

    # Ping failed
    FAIL_COUNT=$((FAIL_COUNT + 1))
    log "⚠ Wi-Fi packet drop detected (cycle $FAIL_COUNT). Target: $TARGET unreachable."

    # Level 1 recovery: Soft reassociate (after 15-30s downtime)
    if [ $FAIL_COUNT -eq 2 ]; then
        log "Attempting soft Wi-Fi reassociation..."
        if command -v nmcli &>/dev/null; then
            nmcli device reapply "$INTERFACE" 2>/dev/null || nmcli device connect "$INTERFACE" 2>/dev/null || true
        elif command -v wpa_cli &>/dev/null; then
            wpa_cli -i "$INTERFACE" reassociate 2>/dev/null || true
        fi
    fi

    # Level 2 recovery: Connect to backup mobile hotspot if configured (after 45s)
    if [ $FAIL_COUNT -eq 3 ] && [ -f "$CONFIG_FILE" ]; then
        BACKUP_SSID=$(grep -E '^BACKUP_SSID=' "$CONFIG_FILE" | cut -d= -f2- | tr -d '"')
        BACKUP_PASS=$(grep -E '^BACKUP_PASS=' "$CONFIG_FILE" | cut -d= -f2- | tr -d '"')
        if [ -n "$BACKUP_SSID" ]; then
            log "Primary Wi-Fi down >45s. Attempting failover to backup hotspot: $BACKUP_SSID..."
            if command -v nmcli &>/dev/null; then
                nmcli dev wifi connect "$BACKUP_SSID" password "$BACKUP_PASS" ifname "$INTERFACE" 2>/dev/null || true
            fi
        fi
    fi

    # Level 3 recovery: Hardware interface bounce (after 60s downtime)
    if [ $FAIL_COUNT -ge $MAX_RETRIES_BEFORE_BOUNCE ]; then
        log "Interface $INTERFACE unresponsive for >60s. Bouncing wireless interface..."
        ip link set "$INTERFACE" down 2>/dev/null || true
        sleep 2
        ip link set "$INTERFACE" up 2>/dev/null || true
        disable_power_save
        if command -v nmcli &>/dev/null; then
            nmcli device connect "$INTERFACE" 2>/dev/null || true
        fi
        FAIL_COUNT=0
    fi
done
