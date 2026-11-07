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
# Standard 14-day dry run with default $100,000 USDT capital (pure live Binance feeds, fallback off):
piquant dry-run

# Opt-in to local synthetic fallback during network outages (paper PnL & quoting paused while offline):
piquant dry-run --fallback

# Specifying custom Binance WebSocket gateway (e.g. US or direct vision feed):
piquant dry-run --ws-url "wss://data-stream.binance.vision:9443/stream?streams=btcusdt@depth20@100ms/ethusdt@depth20@100ms/btcusdt@trade/ethusdt@trade"

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
 Data Source:     live
 Feed Down Time:  0.0s
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

## 3. Data Integrity & Realistic Execution Architecture

### Pure Live Feed Isolation & Fallback Policy
1. **Default Opt-In (`--fallback=false`)**: By default, `piquant dry-run` runs strictly on live exchange feeds. If network drops occur, the engine will NOT generate synthetic ticks unless `--fallback` is explicitly provided.
2. **PnL Freezing During Feed Down**: While in fallback mode, quoting and order fills are strictly paused, mark prices remain frozen, and cumulative offline duration is tracked under `feed_down_seconds`.
3. **Data Source Tagging**: Every executed trade in `data/paper/dryrun_journal.jsonl`, the `/api/paper/dryrun` payload, the blotter UI banner, and CLI `--status` are explicitly tagged with `data_source = live|fallback`.

### Realistic Paper Matching & Queue-Reactive Modeling
- **Queue-Priority Fill Realism**: Replaces naive optimistic fills. A resting maker order fills only when observed market trades at or through its price deplete the preceding queue (`hftbacktest` fill realism).
- **Latency Simulation**: Maker orders are subject to configurable network latency (`OrderLatency`, default 50ms) before participating in matching.
- **Queue-Reactive Intensity Model**: Employs the Huang-Lehalle-Rosenbaum binned queue state model (arXiv:2501.08822) to compute continuous fill survival probabilities in `SmartOrderRouter`.
- **Ablation Gate**: Models are screened out-of-sample after fees vs the `mlofi_ridge` baseline. Underperforming models are disabled by default in the ensemble (re-enable via `--enable-all-models`).

---

## 4. Raspberry Pi 5 Micro-Latency Stress Harness

Evaluate full-path micro-latency (`ring -> book -> features -> models -> paper engine`) under Pi 5 hardware constraints (taskset -c 0-3, GOMAXPROCS=4, GOMEMLIMIT=2GiB) with a 4.0x Pi slowdown factor:

```bash
# Run complete stress harness across 1x (100 ev/s), 10x (1,000 ev/s), and burst (>20k ev/s)
./scripts/pi5-stress.sh all

# Or run directly via piquant CLI
./bin/piquant bench --events 5000 --mode all
```

**Pass Criteria**: Pi 5 4x p99 latency < 1.0 ms and zero ring drops at 10x pressure.

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
