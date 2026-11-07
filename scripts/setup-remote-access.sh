#!/usr/bin/env bash
# ==============================================================================
# PiQuant Multi-Network Remote Access Setup (Tailscale & Cloudflare Tunnel)
# ==============================================================================
# Allows seamless dashboard access and SSH to your Raspberry Pi when your
# phone, laptop, or PC are on DIFFERENT Wi-Fi networks (school, cafe, 5G hotspot).
# ==============================================================================
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

echo -e "${BOLD}${CYAN}=======================================================${NC}"
echo -e "${BOLD}${CYAN}  PiQuant Cross-Network Remote Access Setup${NC}"
echo -e "${BOLD}${CYAN}=======================================================${NC}"
echo ""
echo "Select your preferred remote access method:"
echo "  1) Tailscale Mesh VPN (Recommended: fast, encrypted WireGuard, SSH + Web)"
echo "  2) Cloudflare Quick Tunnel (Free public HTTPS URL, no app needed on phone)"
echo "  3) Install Both"
echo ""
read -r -p "Enter choice [1-3] (default: 1): " CHOICE
CHOICE=${CHOICE:-1}

# --- OPTION 1: TAILSCALE ---
setup_tailscale() {
    echo -e "\n${BOLD}${CYAN}--> Configuring Tailscale Mesh Network...${NC}"
    if ! command -v tailscale &>/dev/null; then
        echo -e "${YELLOW}Tailscale not found. Installing via official script...${NC}"
        curl -fsSL https://tailscale.com/install.sh | sh
    else
        echo -e "${GREEN}Tailscale is already installed.${NC}"
    fi

    echo -e "${CYAN}Starting Tailscale with SSH enabled...${NC}"
    echo -e "${YELLOW}Please authenticate by opening the URL shown below in any browser:${NC}"
    sudo tailscale up --ssh --hostname="piquant-pi5" --accept-routes || true

    TAIL_IP=$(tailscale ip -4 2>/dev/null || echo "")
    if [ -n "$TAIL_IP" ]; then
        echo -e "\n${BOLD}${GREEN}✔ Tailscale is ACTIVE!${NC}"
        echo -e "Your Pi's Permanent Tailscale IP: ${BOLD}${GREEN}${TAIL_IP}${NC}"
        echo -e "Dashboard URL from ANY Wi-Fi:   ${BOLD}${CYAN}http://${TAIL_IP}:8080${NC}"
        echo -e "MagicDNS URL:                   ${BOLD}${CYAN}http://piquant-pi5:8080${NC}"
        echo -e "SSH from ANY Wi-Fi:             ${BOLD}${CYAN}ssh $(whoami)@${TAIL_IP}${NC}"
    fi
}

# --- OPTION 2: CLOUDFLARE TUNNEL ---
setup_cloudflare() {
    echo -e "\n${BOLD}${CYAN}--> Configuring Cloudflare Tunnel...${NC}"
    if ! command -v cloudflared &>/dev/null; then
        echo -e "${YELLOW}Installing cloudflared...${NC}"
        ARCH=$(uname -m)
        if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
            CF_PKG="cloudflared-linux-arm64.deb"
        elif [ "$ARCH" = "armv7l" ]; then
            CF_PKG="cloudflared-linux-arm.deb"
        else
            CF_PKG="cloudflared-linux-amd64.deb"
        fi
        wget -q "https://github.com/cloudflare/cloudflared/releases/latest/download/${CF_PKG}" -O "/tmp/${CF_PKG}"
        sudo dpkg -i "/tmp/${CF_PKG}" || sudo apt-get install -f -y
        rm -f "/tmp/${CF_PKG}"
    else
        echo -e "${GREEN}cloudflared is already installed.${NC}"
    fi

    # Create systemd service for persistent quick tunnel
    echo -e "${CYAN}Creating systemd service for persistent background tunnel...${NC}"
    sudo tee /etc/systemd/system/piquant-tunnel.service >/dev/null <<EOF
[Unit]
Description=PiQuant Cloudflare Remote Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$(whoami)
ExecStart=/usr/local/bin/cloudflared tunnel --url http://127.0.0.1:8080
Restart=always
RestartSec=5
StandardOutput=append:/var/log/piquant-tunnel.log
StandardError=append:/var/log/piquant-tunnel.log

[Install]
WantedBy=multi-user.target
EOF

    sudo systemctl daemon-reload
    sudo systemctl enable piquant-tunnel.service
    sudo systemctl restart piquant-tunnel.service

    sleep 3
    TUNNEL_URL=$(grep -o 'https://[-a-zA-Z0-9.]*\.trycloudflare\.com' /var/log/piquant-tunnel.log 2>/dev/null | tail -n 1 || echo "")
    echo -e "\n${BOLD}${GREEN}✔ Cloudflare Tunnel Service Started!${NC}"
    if [ -n "$TUNNEL_URL" ]; then
        echo -e "Your Public HTTPS URL: ${BOLD}${GREEN}${TUNNEL_URL}${NC}"
    else
        echo -e "Check your assigned URL anytime with: ${CYAN}grep -o 'https://.*trycloudflare.com' /var/log/piquant-tunnel.log | tail -1${NC}"
    fi
}

case "$CHOICE" in
    1) setup_tailscale ;;
    2) setup_cloudflare ;;
    3) setup_tailscale; setup_cloudflare ;;
    *) setup_tailscale ;;
esac

echo -e "\n${BOLD}${GREEN}✔ Remote access configuration complete!${NC}"
echo "You can now monitor your 14-day dry run from your phone or laptop on any Wi-Fi."
