# 2-Week Autonomous Paper Money Dry Run Operator Guide

This guide details how to launch, monitor, verify, and maintain the **14-Day (336-Hour) Autonomous Paper Money Dry Run** on Raspberry Pi 5 (and dual-node Pi 5 + Pi 3B clusters).

---

## 1. Overview & Architecture

The 14-Day Autonomous Paper Money Dry Run evaluates PiQuant's quantitative market-making and statistical arbitrage engines in real-time under live market conditions without risking capital.

- **Initial Paper Capital**: \$100,000.00 USDT.
- **Target Duration**: 14 Days / 336 Hours continuous run.
- **Universe**: BTCUSDT and ETHUSDT.
- **Data Ingestion**: 100% public Binance WebSocket multi-stream (`@depth20@100ms`, `@trade`) requiring no API keys or exchange credentials.
- **Lock-Free Queue**: Ingested ticks are streamed into `engine.SPSCRing` across pinned CPU cores (isolcpus 2,3) with zero heap allocations (`0 B/op, 0 allocs/op`).
- **Signal Stack**:
  - `SplitConformalCalibrator`: 90% finite-sample non-parametric coverage gating execution.
  - `GLFTMarketMaker` & `CarteaJaimungalMM`: Dynamic Avellaneda–Stoikov & HJB queue-reactive reservation quotes.
  - `AdaHedgeEnsemble`: Adaptive multi-expert online meta-portfolio.
  - `MicrostructureCircuitBreaker`: Halts immediately if spread or VPIN exceeds extreme bounds or max drawdown reaches 2.0%.
  - `PageHinkleyDriftGuard`: Monitors sequential change-point residuals to back off during structural regime breaks.
- **Persistent State Checkpointing**: Seamlessly writes state to `data/paper/dryrun_state.json` and appends executed trades to `data/paper/dryrun_journal.jsonl`. If power is lost or the Raspberry Pi reboots, the engine seamlessly resumes the 14-day clock and portfolio without data loss.

---

## 2. Quick Start

### Starting the Dry Run via CLI

To run interactively in the foreground:

```bash
# Standard 14-day dry run with default $100,000 USDT capital:
piquant dry-run

# Customizing parameters:
piquant dry-run --capital 100000 --duration-hours 336 --max-drawdown 2.0 --addr :8080
```

### Checking Status from Terminal

At any time, operators can query the active dry run status from the command line:

```bash
piquant dry-run --status
```

Sample output:
```text
=======================================================
 PIQUANT 2-WEEK AUTONOMOUS DRY RUN STATUS REPORT
=======================================================
 Status:          ACTIVE (OK)
 Progress:        Day 1 of 14 (334.8 hours remaining)
 Start Time:      2026-11-04T18:30:00Z
 Target End:      2026-11-18T18:30:00Z
 Initial Capital: $100000.00 USDT
 Current Equity:  $100248.50 USDT (+0.25%)
 Cash:            $98450.20 USDT
 Realized PnL:    $185.40 USDT
 Unrealized PnL:  $63.10 USDT
 Peak Equity:     $100262.10 USDT
 Max Drawdown:    0.08%
 Total Trades:    42 (Win Rate: 64.3%)
-------------------------------------------------------
 Open Inventory Positions:
   BTCUSDT: Size=0.1500 Entry=$49980.50 Mark=$50000.00 UnrealizedPnL=$2.92 Trades=26
   ETHUSDT: Size=0.5000 Entry=$3395.00 Mark=$3400.12 UnrealizedPnL=$2.56 Trades=16
=======================================================
```

---

## 3. Web Dashboard & Blotter

Access the real-time visual blotter by navigating to `http://<pi5-ip>:8080` in any web browser and entering your dashboard token.

### 2-Week Dry Run Banner
Located at the top of the Blotter:
```text
[2-WEEK DRY RUN ACTIVE: Day 1 of 14 • 335h left] Equity: $100,248.50 (+0.25%) | Max DD: 0.08% | Win Rate: 64% | Circuit Breaker: OK
```

### Real-Time Metrics & Rest API
- `GET /api/paper/dryrun`: Returns complete JSON state including remaining time, positions, win rate, Sharpe ratio, and recent fills.
- `GET /api/paper/blotter`: Returns live GLFT reservation skews, Hawkes flow imbalance, BOCPD regime probabilities, and Cont-de Larrard queue fill probabilities.

---

## 4. Production Systemd Service Installation (Raspberry Pi 5)

To run the dry run 24/7 as an autonomous daemon that restarts across power cycles and reboots:

1. Copy the unit file to systemd:
   ```bash
   sudo cp deploy/piquant-dryrun.service /etc/systemd/system/
   sudo systemctl daemon-reload
   ```

2. Enable and start the service:
   ```bash
   sudo systemctl enable --now piquant-dryrun
   ```

3. Verify service and CPU affinity pinning:
   ```bash
   sudo systemctl status piquant-dryrun
   journalctl -u piquant-dryrun -f
   ```

---

## 5. Circuit Breaker & Safety Guarantees

- **Max Drawdown Gate**: If portfolio drawdown reaches **2.0%** from peak equity, the `DryRunEngine` halts all new orders immediately and flags `circuit_halted: true`.
- **Microstructure Circuit Breaker**: If order book spread blows out beyond median threshold or VPIN toxicity spikes, quotes are pulled automatically.
- **Fail-Safe Fallback**: If the internet connection drops or Binance rate-limits, the `MarketStreamIngestor` activates its local fallback generator to prevent engine stalls, then automatically reconnects with exponential backoff.
