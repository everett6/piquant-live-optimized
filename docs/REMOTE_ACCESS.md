# PiQuant Remote Access, Network Resilience & UI Guide

This guide addresses multi-network access, Wi-Fi failover, autonomous paper tracking, and research engine configuration.

---

## 1. Accessing Your Pi Across Different Wi-Fis

When your Raspberry Pi is at home and your laptop or phone is on another network (school, work, coffee shop, or cellular 5G hotspot), local IP addresses like `192.168.0.x` cannot route over the Internet.

PiQuant provides **two zero-configuration solutions**:

### Method A: Tailscale Mesh VPN (Recommended)
Tailscale creates an encrypted, peer-to-peer WireGuard mesh between your Pi, phone, laptop, and PC:

1. On the Pi, run the setup script:
   ```bash
   ./scripts/setup-remote-access.sh
   ```
   Select **Option 1 (Tailscale)**. Authenticate once via the URL provided.
2. Install the free **Tailscale** app on your iPhone, Android, MacBook, or Windows/Linux laptop, and log in with the same account.
3. Your Pi is now assigned a permanent Tailscale IP (e.g. `100.81.116.48`).
4. **Open the Dashboard from anywhere in the world:**
   ```text
   http://piquant-pi5:8080   or   http://<YOUR_TAILSCALE_IP>:8080
   ```
5. **SSH from anywhere with no port forwarding:**
   ```bash
   ssh everett@<YOUR_TAILSCALE_IP>
   ```

---

### Method B: Cloudflare Quick Tunnel (Zero-Install Browser Access)
If you do not want to install any app on your phone:
1. Run `./scripts/setup-remote-access.sh` and select **Option 2 (Cloudflare Tunnel)**.
2. Cloudflare assigns a secure, public HTTPS URL:
   ```text
   https://piquant-live-xxxx.trycloudflare.com
   ```
3. Open that URL on any device anywhere in the world, enter your dashboard token, and access the live mission control.
4. It runs automatically in the background via `deploy/piquant-tunnel.service`.

---

## 2. What Happens When Wi-Fi Drops?

PiQuant features **two layers of defense** against Wi-Fi drops:

### Layer 1: Hardware & Network Watchdog (`scripts/wifi-watchdog.sh`)
* **Disables Wi-Fi Power Saving:** Raspberry Pi Wi-Fi chips often sleep during idle periods and fail to reconnect. The watchdog permanently forces `iw wlan0 set power_save off`.
* **Probes Every 15s:** Pings the gateway and `1.1.1.1`.
* **Automatic Reconnect:**
  * **15–30s offline:** Triggers `nmcli device reapply` or `wpa_cli reassociate`.
  * **45s offline:** Automatically scans for a **Backup Mobile Hotspot** (e.g. your iPhone/Android hotspot configured in `/etc/piquant/backup-wifi.conf`) and connects to it.
  * **60s offline:** Performs a clean hardware interface bounce (`ip link set wlan0 down && ip link set wlan0 up`).

To enable the watchdog 24/7 on your Pi:
```bash
sudo cp deploy/piquant-wifi-watchdog.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now piquant-wifi-watchdog.service
```

### Layer 2: PiQuant Capital Protection Safeguard
When Wi-Fi drops, the WebSocket connection disconnects:
1. **Quoting Paused Instantly:** `DryRunEngine` sets `QuotingPaused = true`.
2. **Resting Orders Cancelled:** Bids and asks are immediately pulled from the market simulator.
3. **Fills Frozen:** No simulated fills can execute on missing or stale market prices. Your portfolio is completely protected from stale fills or cascade slippage.
4. **Outage Duration Tracked:** `FeedDownSeconds` records downtime for institutional audit.
5. **Instant Resumption:** When Wi-Fi restores, the public Binance WebSocket reconnects with exponential backoff, recalculates L2 depth, and resumes quoting with zero lost state.

---

## 3. The 14-Day Autonomous Dry Run Mission Control

The dashboard features **two dedicated locations** to track your money:

### 1. Overview Tab — Top Mission Control Card (Above the Fold)
Placed prominently at the very top of the Overview tab:
* **Portfolio Equity:** Real-time mark-to-market portfolio value (e.g. `$100,042.80 USDT`).
* **Realized PnL & Return:** Bold PnL in dollars and percent (`+$42.80 (+0.04%)`).
* **14-Day Progress Bar:** Visual segmented meter (`Day 1 of 14 • 334.8h remaining`).
* **Market Stream Throughput:** Real-time throughput indicator:
  * **BTCUSDT:** `~500 events/sec` (100ms L2 orderbook depth + aggressive trade prints).
  * **ETHUSDT:** `~100–300 events/sec`.
  * **Total Ingested Events:** Microsecond event counter with 0 dropped packets.
* **Risk & Circuit Breaker Pills:** `CIRCUIT BREAKER: NOMINAL (0.02% DD / 2.00% Limit)`.
* **Feed & Network Status:** `FEED: BINANCE WEBSOCKET LIVE` & `WIFI / LAN: CONNECTED`.

### 2. Dedicated "Paper Trading" Tab (`tab-dryrun`)
Click **"Paper Trading"** in the top navigation bar to open the full execution desk:
* **Live Blotter Table:** Streaming fills with timestamps, symbols, sides, prices, quantities, edge bps, adverse selection bps, and passive maker rebates.
* **Microstructure Gauges:** VPIN toxicity, Hawkes cascade branching ratio, Cross-venue lead-lag spread, and inventory holding age decay.

---

## 4. "Research Engine Not Found" — Resolved

### Why It Appeared:
In dual-node mode (Pi 5 + Pi 3B), the Pi 3B dashboard looks across the LAN for a separate Pi 5 engine peer. If running on a single Pi 5 in standalone mode, or if `opts.Manager` was not initialized in `dry-run`, the UI reported `"research engine reachable: no engine answered on the local network"`.

### The Fix:
* `cmd/piquant/dryrun.go` now initializes the local experiment manager `experiment.NewManager` and co-locates the research engine directly with the paper trading dashboard.
* The dashboard auto-detects `ClusterMode: "pi5_only"`, setting `engineLive = true` and `"standalone mode: running engine and dashboard co-located on Raspberry Pi 5"`.
* The hero banner dynamically updates to `14-Day Autonomous Dry Run — ACTIVE` with zero missing peer errors.
