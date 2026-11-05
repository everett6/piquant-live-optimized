#!/usr/bin/env bash
# PiQuant — Production Node Provisioning and Installation Script
# Supports Raspberry Pi 5 Standalone, Pi 5 Engine, and Raspberry Pi 3B Dashboard
set -euo pipefail

ROLE="${1:-auto}"
MODE="${2:-auto}" # auto | standalone (pi5 only) | dual (pi5 + 3b)
PREFIX="${PREFIX:-/usr/local}"
PIQUANT_USER="piquant"
CONFIG_DIR="/etc/piquant"
DATA_DIR="/var/lib/piquant/data"
LOG_DIR="/var/log/piquant"

echo "=== PiQuant Production Installer ==="
echo "Modes supported: 'standalone' (RPi 5 runs research engine + UI co-located)"
echo "                 'dual' (RPi 5 engine + RPi 3B dashboard & telemetry)"

# Check root privilege
if [[ $EUID -ne 0 ]]; then
   echo "Error: This script must be run as root (e.g. sudo ./scripts/install.sh)" >&2
   exit 1
fi

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  aarch64)
    GOARCH="arm64"
    GOARM=""
    DEFAULT_ROLE="agent"
    ;;
  armv7l|armv6l)
    GOARCH="arm"
    GOARM="7"
    DEFAULT_ROLE="dashboard"
    ;;
  x86_64)
    GOARCH="amd64"
    GOARM=""
    DEFAULT_ROLE="agent"
    ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    exit 2
    ;;
esac

if [[ "$ROLE" == "auto" ]]; then
  ROLE="$DEFAULT_ROLE"
fi

if [[ "$ROLE" != "dashboard" && "$ROLE" != "agent" ]]; then
  echo "Error: Invalid role '$ROLE'. Must be 'dashboard' (Pi 3B) or 'agent' (Pi 5)." >&2
  exit 2
fi

echo "Detected architecture: $ARCH (building for GOARCH=$GOARCH${GOARM:+ GOARM=$GOARM})"
echo "Selected role: $ROLE"

# 1. Create dedicated system user
if ! id "$PIQUANT_USER" &>/dev/null; then
  echo "Creating system user $PIQUANT_USER..."
  useradd -r -s /usr/sbin/nologin -d /var/lib/piquant -M "$PIQUANT_USER" || true
fi

# 2. Create directory hierarchy
echo "Setting up filesystem directories..."
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" /var/lib/piquant
chown -R "$PIQUANT_USER:$PIQUANT_USER" "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" /var/lib/piquant
chmod 0700 "$CONFIG_DIR"
chmod 0755 "$DATA_DIR" "$LOG_DIR"

# 3. Build binary if needed or copy pre-built binary
ARCH_BIN="bin/piquant-linux-${GOARCH}${GOARM:+v${GOARM}}"
if [[ -f "$ARCH_BIN" ]]; then
  echo "Installing pre-built binary $ARCH_BIN to $PREFIX/bin/piquant..."
  install -Dm755 "$ARCH_BIN" "$PREFIX/bin/piquant"
elif [[ -f "bin/piquant-linux-$GOARCH" ]]; then
  echo "Installing pre-built binary bin/piquant-linux-$GOARCH to $PREFIX/bin/piquant..."
  install -Dm755 "bin/piquant-linux-$GOARCH" "$PREFIX/bin/piquant"
elif [[ -f "bin/piquant" ]]; then
  echo "Installing existing binary bin/piquant to $PREFIX/bin/piquant..."
  install -Dm755 bin/piquant "$PREFIX/bin/piquant"
elif command -v go &>/dev/null; then
  echo "Building piquant binary..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" GOARM="$GOARM" \
    go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo '0.2.0-prod')" \
    -o "$PREFIX/bin/piquant" ./cmd/piquant
  chmod 755 "$PREFIX/bin/piquant"
else
  echo "Error: 'go' toolchain not found and bin/piquant not present." >&2
  echo "Please build with 'make build' or install Go on this host first." >&2
  exit 3
fi

# 4. Provision initial secrets if missing
SECRETS_FILE="$CONFIG_DIR/secrets"
if [[ ! -f "$SECRETS_FILE" ]]; then
  echo "Generating initial node tokens in $SECRETS_FILE..."
  sudo -u "$PIQUANT_USER" "$PREFIX/bin/piquant" setup --role "$ROLE" --dir "$CONFIG_DIR" --data "$DATA_DIR" --no-discovery
  chmod 0600 "$SECRETS_FILE"
fi

# 5. Low-latency Linux kernel & hardware tuning
if [[ "$ROLE" == "agent" && -f "scripts/tune-kernel.sh" ]]; then
  echo ""
  echo "Applying low-latency kernel tuning (isolated cores, sysctl busy-polling, performance governor)..."
  chmod +x scripts/tune-kernel.sh
  ./scripts/tune-kernel.sh apply || echo "Warning: Kernel tuning partially applied (non-fatal)."
fi

# 6. Install systemd service
SERVICE_NAME="piquant-$ROLE.service"
echo "Installing systemd service $SERVICE_NAME..."
install -Dm644 "deploy/$SERVICE_NAME" "/etc/systemd/system/$SERVICE_NAME"

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

echo ""
echo "=== Installation Successful ==="
echo "Service: $SERVICE_NAME is active"
echo "Role:    $ROLE"
echo "Status:  sudo systemctl status $SERVICE_NAME"
echo "Logs:    $LOG_DIR/"
echo "Secrets: $SECRETS_FILE"
echo ""
if [[ "$ROLE" == "dashboard" ]]; then
  echo "Access the dashboard at http://piquant.local:8080 or http://<this-pi-ip>:8080"
  echo "Operating in Dual-Node Mode with Raspberry Pi 5 core."
else
  echo "PiQuant Research Engine running on Raspberry Pi 5."
  echo "  - Pi 5 Standalone UI: http://piquant-core.local:8080"
  echo "  - Dual-Node Mode: Discovers Pi 3B at http://piquant.local:8080 automatically via mDNS."
fi
