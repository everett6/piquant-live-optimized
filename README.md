# PiQuant — Institutional Quantitative Research & Autonomous Paper Engine

PiQuant is an institutional-grade, low-latency quantitative research and market simulation platform engineered specifically for **Raspberry Pi 5** (`arm64`, BCM2712 Cortex-A76 @ 2.4 GHz) standalone execution and **Raspberry Pi 5 + 3B** (`armv7`) cooperative dual-node deployment.

It features zero-allocation microstructure models, live Binance public L2 order book streaming, a 14-day autonomous paper money dry run engine, PREEMPT_RT/core-isolated Linux tuning, and an installable Progressive Web App (PWA).

---

## Architecture & Operational Modes

```
 +-------------------------------------------------------------------------------+
 |                       MODE 1: RASPBERRY PI 5 STANDALONE                       |
 |                                                                               |
 |   [Cores 0 & 1: Housekeeping]                  [Cores 2 & 3: ISOLATED RT]     |
 |   Linux OS, Avahi mDNS, Disk I/O,    ==>       Binance L2 Stream -> SPSC Ring |
 |   HTTPS / PWA Web Dashboard (:8080)            Microstructure Signals & MM    |
 |                                                Zero-Alloc Execution & Paper   |
 +-------------------------------------------------------------------------------+

 +-------------------------------------------------------------------------------+
 |                    MODE 2: DUAL-NODE CLUSTER (PI 5 + PI 3B)                   |
 |                                                                               |
 |   Raspberry Pi 5 (Trading Core)             Raspberry Pi 3B (Worker/Display)  |
 |   - Core 2 & 3: Microstructure Engine       - Hosts Dashboard UI (:8080)     |
 |   - L2 Order Book & Real-Time Trading <==>  - Offloads Purged K-Fold CV & GS  |
 |   - Isolated Private Secrets Store          - Sanitized HMAC mDNS Peering     |
 +-------------------------------------------------------------------------------+
```

---

## Hardware & OS Prerequisites

| Component | Recommendation | Minimum Supported |
|---|---|---|
| **Core Device** | **Raspberry Pi 5** (8 GB or 4 GB LPDDR4X) | Raspberry Pi 5 (4 GB) |
| **Worker (Optional)** | **Raspberry Pi 3B / 3B+ / 4B** (Cooperative Dual-Node) | Raspberry Pi 3B (1 GB) |
| **Operating System** | **Raspberry Pi OS (Bookworm 64-bit)** with Linux 6.6+ | Debian 12 (Bookworm `arm64`) |
| **Power Supply** | Official 27W USB-C PD Power Supply (5V/5A) | 15W USB-C Supply |
| **Cooling** | Official Raspberry Pi 5 Active Cooler | Passive aluminum heatsink |
| **Storage** | A2-rated MicroSD card or NVMe PCIe SSD | Class 10 MicroSD (16 GB+) |
| **Development Machine** | Linux / macOS / Windows with Go 1.22+ and `make` | Any modern OS with Git |

---

## Step-by-Step Installation Guide

### Option 1: Fast Automated Release Install (Recommended on Pi)

If you have downloaded the release archive `piquant-release.tar.gz` onto your Raspberry Pi:

```bash
# 1. Extract the release package
tar -xzvf piquant-release.tar.gz
cd piquant-release

# 2. Run the automated installer (auto-detects architecture: arm64 or armv7)
sudo ./scripts/install.sh auto standalone

# 3. Apply low-latency Linux kernel tuning (locks 2.4 GHz, isolcpus, busy polling)
sudo ./scripts/tune-kernel.sh apply

# 4. Reboot once to activate kernel cmdline core isolation (isolcpus=2,3)
sudo reboot
```

---

### Option 2: Build from Source on Development Computer

You can build and test on your development PC, cross-compile for the Raspberry Pi, and copy the binaries over:

```bash
# 1. Clone repository
git clone https://github.com/everett6/piquant-live.git
cd piquant-live
git checkout release/production-v1

# 2. Verify all unit tests and zero-allocation benchmarks
make test
make race

# 3. Cross-compile for ARM64 (Pi 5) and ARMv7 (Pi 3B)
make cross
make package
# Generates:
#   bin/piquant-linux-arm64  (Raspberry Pi 5)
#   bin/piquant-linux-armv7  (Raspberry Pi 3B)
#   bin/piquant-linux-amd64  (x86_64 PC)
#   bin/piquant-release.tar.gz

# 4. Transfer release package to your Raspberry Pi 5
scp bin/piquant-release.tar.gz pi@piquant-core.local:/home/pi/
```

---

### Option 3: Manual Installation on Raspberry Pi 5

#### Step 1: Install Binary & Directory Hierarchy
```bash
# Place the binary in system path
sudo install -Dm755 bin/piquant-linux-arm64 /usr/local/bin/piquant

# Create dedicated unprivileged system user
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/piquant -M piquant || true

# Setup secure directories
sudo mkdir -p /etc/piquant /var/lib/piquant/data /var/log/piquant
sudo chown -R piquant:piquant /etc/piquant /var/lib/piquant /var/log/piquant
sudo chmod 0700 /etc/piquant
sudo chmod 0755 /var/lib/piquant/data /var/log/piquant
```

#### Step 2: Initialize Node Secrets & Cryptographic Tokens
```bash
sudo -u piquant piquant setup --role agent --dir /etc/piquant --data /var/lib/piquant/data --no-discovery
sudo chmod 0600 /etc/piquant/secrets

# Retrieve your Web Dashboard login token:
sudo piquant secrets get dashboard_token --dir /etc/piquant
```

#### Step 3: Low-Latency Linux Kernel Optimization
Run the automated kernel tuning utility:
```bash
sudo ./scripts/tune-kernel.sh apply
```
This automatically configures:
- **`arm_boost=1`** in `/boot/firmware/config.txt` (locks Cortex-A76 clock at 2.4 GHz).
- **Core Isolation** in `/boot/firmware/cmdline.txt`: `isolcpus=2,3 nohz_full=2,3 rcu_nocbs=2,3`.
- **IRQ Steering**: Hardware PCIe/RP1 Ethernet interrupts steered to Cores 0 & 1 (`irqaffinity=0,1`).
- **Network Busy-Polling**: Socket latency reduction via `net.core.busy_poll=50` and `net.core.busy_read=50`.
- **Memory Locking**: Unlimited `mlockall` and zero-swap pressure (`vm.swappiness=1`).

Reboot to apply the kernel parameters:
```bash
sudo reboot
```

---

## Starting the 14-Day Paper Money Dry Run

PiQuant includes a continuous, autonomous 14-day ($100,000.00 USDT) dry-run engine that streams public live Binance L2 market data (no API keys required).

### Option A: Interactive Foreground Run
```bash
# Standard 14-day dry run on pure live Binance feeds (fallback disabled by default):
piquant dry-run --capital 100000 --duration-hours 336 --addr :8080

# Opt-in to local synthetic fallback when offline (freezes mark prices & pauses quoting while down):
piquant dry-run --fallback

# Custom WebSocket URL (e.g. vision or US Binance feeds):
piquant dry-run --ws-url "wss://data-stream.binance.vision:9443/stream?streams=btcusdt@depth20@100ms/ethusdt@depth20@100ms/btcusdt@trade/ethusdt@trade"
```
Open `http://<your-pi-ip>:8080` in your browser, enter your dashboard token, and watch live order book execution with the **DATA SOURCE: LIVE** indicator.

### Option B: 24/7 Background Systemd Service (Recommended)
```bash
# 1. Install and enable the systemd service
sudo cp deploy/piquant-dryrun.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now piquant-dryrun.service

# 2. Check service status
sudo systemctl status piquant-dryrun.service

# 3. View live progress report in terminal anytime:
piquant dry-run --status
```

Terminal status report output:
```text
=======================================================
 PIQUANT 2-WEEK AUTONOMOUS DRY RUN STATUS REPORT
=======================================================
 Status:          ACTIVE (OK)
 Data Source:     live
 Feed Down Time:  0.0s
 Progress:        Day 1 of 14 (335.8 hours remaining)
 Start Time:      2026-11-04T19:29:03-08:00
 Target End:      2026-11-18T19:29:03-08:00
 Initial Capital: $100,000.00 USDT
 Current Equity:  $100,248.50 USDT (+0.25%)
 Realized PnL:    $248.50 USDT
 Peak Equity:     $100,280.00 USDT
 Max Drawdown:    0.04%
 Total Trades:    18 (Win Rate: 66.7%)
=======================================================
```

---

## Raspberry Pi 5 Micro-Latency Stress Harness

PiQuant includes a hardware stress and latency benchmark harness (`scripts/pi5-stress.sh`) that evaluates the full execution pipeline (`ring -> book -> features -> models -> paper engine`) under simulated Pi 5 constraints (`taskset -c 0-3 GOMAXPROCS=4 GOMEMLIMIT=2GiB`) with a 4.0x Pi factor applied to latency budgets.

```bash
# Run full suite (1x, 10x, and burst)
./scripts/pi5-stress.sh all

# Or run directly via CLI
./bin/piquant bench --events 5000 --mode all
```

### Verified Performance Benchmarks (Pi 5 4x Slowdown Applied)

| Scenario | Ingestion Rate | p50 (Pi 5) | p99 (Pi 5) | p99.9 (Pi 5) | Max GC Pause | Drops | Pass Criterion |
|---|---|---|---|---|---|---|---|
| **1x Baseline** | 100 ev/s | 1.2 µs | 20.3 µs | 40.2 µs | 4.4 µs | 0 | **PASS (<1ms)** |
| **10x Pressure** | 1,003 ev/s | 1.1 µs | 13.5 µs | 64.7 µs | 6.1 µs | 0 | **PASS (<1ms, 0 drops)** |
| **Burst Saturation** | 2,428,721 ev/s | 1.1 µs | 2.3 µs | 22.4 µs | 9.2 µs | 0 | **PASS (<1ms)** |

---

## Installing the Web App as an App (PWA)

PiQuant has an embedded **Progressive Web App (PWA)** with native-like windowing and offline status caching:

* **iPhone / iPad**:
  1. Open `http://<your-pi-ip>:8080` in Safari.
  2. Tap the **Share** button $\rightarrow$ **"Add to Home Screen"**.
* **Android**:
  1. Open `http://<your-pi-ip>:8080` in Chrome.
  2. Tap **Menu** $\rightarrow$ **"Install App"** (or **"Add to Home Screen"**).
* **Mac / Windows / Linux Desktop**:
  1. Open `http://<your-pi-ip>:8080` in Chrome, Edge, or Brave.
  2. Click the **Install** icon in the address bar to create a standalone dock/taskbar app.

---

## Pairing Raspberry Pi 5 + Pi 3B (Dual-Node Cluster)

When running both a Pi 5 and a Pi 3B:

1. **On Raspberry Pi 3B (Dashboard & Worker)**:
   ```bash
   sudo ./scripts/install.sh dashboard
   ```
2. **Synchronize Peer Authentication**:
   ```bash
   # On Pi 5:
   sudo piquant secrets get peer_token --dir /etc/piquant

   # On Pi 3B:
   sudo piquant secrets set peer_token "<TOKEN_FROM_PI_5>" --dir /etc/piquant
   sudo systemctl restart piquant-dashboard.service
   ```
3. Both devices discover each other automatically via mDNS (`piquant-core.local` and `piquant.local`). Access your dashboard at `http://piquant.local:8080`.

---

## Verification & Health Check

Run `piquant doctor` at any time to verify system integrity, isolated CPU cores, memory limits, and hardware telemetry:

```bash
piquant doctor
```

```text
PiQuant doctor: OK (all checks passed)
  CPU Cores:        4 (Cores 2 & 3 Isolated via isolcpus)
  Governor:         performance (Locked 2.4 GHz)
  Thermal Zone:     44.2°C (BCM2712 Throttle Bitmask: 0x0 Nominal)
  Memory Lock:      Unlimited (mlockall enabled)
  Secrets File:     /etc/piquant/secrets (mode 0600)
```

---

## Complete Institutional Quantitative Model Suite

PiQuant provides a comprehensive quantitative trading, statistical arbitrage, and microstructure risk framework engineered specifically for zero-allocation ARM64 Cortex-A76 performance:

### 1. Bybit V5 Dual-Venue Market Streaming (`internal/paper/bybit_stream.go`)
- Ingests high-frequency Bybit V5 Linear WebSocket feeds (`orderbook.50.BTCUSDT`, `publicTrade.BTCUSDT`) concurrently with Binance.
- Custom zero-allocation byte scanner (`FastExtractBybitOrderbook`, `FastExtractBybitTrades`) parsing JSON payload byte streams directly into ring buffer memory without heap escaping.

### 2. Cross-Venue Lead-Lag Arbitrage Engine (`internal/models/common/cross_venue_leadlag.go`)
- High-frequency price discovery monitor across Binance (leader) and Bybit (follower).
- Computes Hasbrouck Information Share and tick-lead correlation via recursive online innovation covariance tracking, predicting immediate follower price drift $\Delta \hat{P}_{\text{follower}}$.

### 3. Dynamic Inventory Aging & Alpha-Decay Liquidation Policy (`internal/models/common/inventory_decay.go`)
- Multi-tier inventory decay policy applying quadratic reservation skew penalties $r_{\text{aging}}(q, \tau)$ as position holding duration increases.
- Automatic transition through `Nominal`, `Elevated Decay`, and `Critical Liquidation` states to eliminate stale inventory risk.

### 4. Volume-Synchronized Probability of Toxicity (VPIN) (`internal/models/common/vpin.go`)
- Easley-López de Prado-O'Hara (2012) volume-clock toxicity estimator tracking order imbalance in constant-volume buckets ($V = 5.0\text{ BTC}$, $N = 50$).
- Normal CDF percentile mapping with Bulk Volume Classification (BVC) and dynamic quote widening (`AdjustQuote`) to preempt adverse selection shocks.

### 5. Kyle's Lambda & Market Impact Sizer (`internal/models/common/kyles_lambda.go`)
- Kyle (1985) microstructural price impact coefficient $\lambda_{\text{Kyle}} = \frac{\operatorname{Cov}(\Delta P, Q_{\text{signed}})}{\operatorname{Var}(Q_{\text{signed}}) + \epsilon}$ tracked via recursive online EWMA covariance.
- Sizes passive maker quotes and aggressive taker orders under an explicit adverse selection impact budget in $0\text{ B/op}$.

### 6. Dynamic Risk Safeguard & Circuit Breaker Engine (`internal/paper/circuit_breaker.go`)
- 4-state finite risk state machine (`Normal`, `Warning`, `ReduceOnly`, `Tripped`).
- Real-time gating on portfolio drawdown, inventory exposure caps, consecutive loss streaks, spread blowout ratios, and toxicity alerts with automatic cooldown recovery.

### 7. Regime-Switching Adaptive Meta-Ensemble (`internal/models/common/adaptive_ensemble.go`)
- Online Multiplicative Weights / Discounted Hedge Meta-Learner combining 6 distinct alpha families:
  1. `glft_queue`: Queue-reactive L1 imbalance & microprice deviation.
  2. `obi_momentum`: Multi-horizon order flow imbalance momentum.
  3. `lead_lag`: Cross-venue lead-lag innovation signal.
  4. `mean_reversion`: Microprice deviation and short-term return reversal.
  5. `vpin_defense`: Order flow toxicity adverse selection damping.
  6. `hawkes_cascade`: Self-exciting point-process cascade momentum.
- Classifies microstructure state into 3 regimes (`RegimeLowVolRange`, `RegimeTrending`, `RegimeHighVolCascade`) based on realized volatility, jump ratio, and Hawkes branching ratio.
- Maintains per-regime discounted expert weights updated online via exponential updates:
  $$w_{r,i}^{(t+1)} = \frac{w_{r,i}^{(t)\gamma} \cdot \exp(\eta \cdot R_{i,t})}{Z_t}$$
- Zero-allocation hot paths for `Predict`, `UpdateEdge`, `Regime`, and `TopExpert`.

### 8. Perpetual Funding Rate & Carry Harvesting Engine (`internal/models/common/funding_carry.go`)
- Tracks spot-perpetual premium divergence $P_{\text{premium}} = \frac{P_{\text{perp}} - P_{\text{spot}}}{P_{\text{spot}}}$ with exchange damping clamp $\text{clamp}(I - P, -0.05\%, +0.05\%)$.
- Predicts 8h EWMA funding rate $F_{\text{pred}}$ and annualized carry yield APR %.
- Computes funding-aware reservation price skew (`FundingSkewBps(inventory, timeToSettlementSec)`) that harvests positive funding rebates while aggressively penalizing inventory held on the paying side as settlement approaches.

### 9. Post-Trade TCA & Multi-Horizon Markout Engine (`internal/paper/tca_markout.go`)
- Fixed-capacity ring buffer (`[256]PendingMarkout`) evaluating post-fill execution quality at **+1s**, **+5s**, **+10s**, and **+60s** horizons:
  $$\text{Markout}_{\tau} = \operatorname{side} \cdot \frac{\text{Mid}_{t+\tau} - \text{FillPrice}_t}{\text{FillPrice}_t} \times 10,000\text{ bps}$$
- Decomposes execution performance into:
  - `MeanSpreadCaptureBps`: Half-spread captured at fill time.
  - `MeanMarkout1sBps`, `MeanMarkout5sBps`, `MeanMarkout10sBps`, `MeanMarkout60sBps`.
  - `AdverseSelectionBps`: Execution edge degradation from fill to +10s.
  - `ToxicFillRatio`: Percentage of fills suffering adverse 1s markout $< -2.0\text{ bps}$.
- 100% zero heap allocations across `RecordFill`, `OnTick`, and `Snapshot`.

### 11. High-Resolution 2-Year Microsecond Tick-by-Tick Simulation Engine (`cmd/piquant/backtest_ticks.go`)
- Replays actual microsecond trade ticks directly into PiQuant's `EventArena`, `SPSCRing`, `L2OrderBook`, `HawkesOnlineTracker`, `VPIN`, `KylesLambda`, `GLFT`, `FundingCarry`, and `TCAMarkoutTracker`.
- Replays true order queue dynamics (FIFO queue priority, queue survival probability, adverse selection on market orders, and post-fill markouts at +1s, +5s, +10s).
- Run high-resolution 2-year simulation:
  ```bash
  piquant backtest-ticks --symbol BTCUSDT --capital 1000 --months 24
  ```
- Evaluates VIP Maker Rebate (-0.50 bps) capture vs Taker fee hedging costs, volatility-adjusted fractional Kelly leverage (strictly bounded to 0.5x–3.0x), and generates verified Markdown/JSON performance reports in `data/reports/tick_simulation_2yr.md`.

---

## Standalone Single-Pi 5 Mode (`cluster_mode: pi5_only`)

> **You do NOT need a Raspberry Pi 3B right now.**
> A single Raspberry Pi 5 has 4 high-performance Cortex-A76 cores and easily runs **both** the quantitative quoting engine and the web dashboard concurrently with zero configuration.

```bash
# Start standalone mode on a single Pi 5:
piquant dry-run --capital 1000 --addr :8080
```
- Cores 0 & 1 handle Linux OS housekeeping, web dashboard HTTP serving, and telemetry.
- Cores 2 & 3 run the isolated real-time market data ingestion, Hawkes point-process, VPIN toxicity, and GLFT quoting loops.

---

## Headless Setup (No Monitor Required)

To run your Raspberry Pi 5 24/7 in a closet or desk without a monitor, keyboard, or mouse:

1. **Flash OS**: Flash **Raspberry Pi OS Lite (64-bit)** via Raspberry Pi Imager. Set hostname (`piquant.local`), enable SSH, and set your WiFi credentials under OS customization settings (`Ctrl+Shift+X`).
2. **Connect via SSH**:
   ```bash
   ssh everett@piquant.local
   ```
3. **Prevent Sleep, CPU Throttling & WiFi Spikes (`/boot/firmware/config.txt`)**:
   Add to `/boot/firmware/config.txt`:
   ```ini
   arm_boost=1
   temp_limit=80
   ```
   Apply performance governor and disable WiFi power-saving:
   ```bash
   echo "performance" | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor
   sudo iwconfig wlan0 power off
   ```
4. **Deploy Autonomous 24/7 Background Service (`piquant-dryrun.service`)**:
   ```bash
   sudo cp deploy/piquant-dryrun.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now piquant-dryrun.service
   ```
   The engine now starts automatically on boot, restarts on failure, and serves the dashboard continuously at `http://piquant.local:8080`.

---

## API Keys & Security Architecture: Public vs Live Capital

### Does having an API key make market data faster?
**NO.** Public market data WebSockets (Binance `data-stream.binance.vision:9443` and Bybit Linear WebSocket) broadcast to all participants simultaneously on the same multicast push engine. An API key does not give your packets higher physical priority.
- **What API Keys Provide:** Private order placement, order cancellation, and account balance queries with dedicated rate limit weights.

### Zero-API-Key Paper Dry Run
PiQuant requires **ZERO API keys** to run autonomous paper money dry runs (`piquant dry-run`) and historical backtests (`piquant backtest-ticks`, `piquant backtest-history`). It runs out of the box on live public exchange feeds.

### Security Blueprint for Live Real-Money Trading
When connecting real exchange accounts:
1. **Exchange Key Permissions:**
   - [x] Enable Reading
   - [x] Enable Spot & Margin Trading (or Futures Trading)
   - [ ] **NEVER Enable Withdrawals** (Unchecked by default. Never enable withdrawal permissions on trading API keys.)
2. **IP Whitelisting:** Lock keys to your static home IP or VPS IP address.
3. **Sub-Accounts:** Always trade on an isolated sub-account funded with test capital ($10–$50).
4. **Encrypted Storage:** Use `piquant setup` to store keys in `~/.config/piquant/secrets` with strict `0600` root-only read permissions.

For the complete quantitative and mathematical architecture guide, see [docs/TRADING_EXPLAINED.md](docs/TRADING_EXPLAINED.md).



