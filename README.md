# PiQuant — Dual-Node Quantitative Research Platform

PiQuant is an offline, high-performance quantitative research and market forecasting platform engineered to run across two dedicated Raspberry Pi devices:

* **Raspberry Pi 5** (`piquant-core.local`): Authoritative Research Computer.
  * Historical market dataset ingestion & versioned JSONL storage.
  * High-performance deterministic historical replay.
  * Streaming zero-alloc feature generation (L1–L20 OFI, microprice deviation, queue ratios, volatility).
  * Model training, out-of-sample evaluation, and hyperparameter search.
  * Experiment scheduling, model registry, and persistent catalog.
  * Authoritative REST/SSE API and fallback web control panel.
* **Raspberry Pi 3B** (`piquant.local`): Dashboard & Lightweight Worker.
  * Hosts the primary scientific analytics research dashboard.
  * Real-time dual-node system telemetry and experiment tracking.
  * Searchable, redacted log viewer.
  * Secure authenticated proxy to the Pi 5 research engine.
  * Lightweight historical evaluation jobs.

> [!IMPORTANT]
> **Safety Boundary:** PiQuant is exclusively an offline research, forecasting, and paper simulation system. It never connects to live financial broker accounts, never executes real-money transactions, and never holds exchange trading credentials.

---

## Zero-Configuration Peering & Discovery

PiQuant eliminates manual IP address and port configuration:
* Devices discover each other automatically via standard Linux **mDNS/DNS-SD (Avahi)** and UDP broadcast beacons on port 8090.
* Predictable hostnames:
  * Pi 5: `piquant-core.local`
  * Pi 3B: `piquant.local`
* Machine-to-machine communication is authenticated via HMAC-SHA256 signed beacons and Bearer tokens stored securely in `~/.config/piquant/secrets` (permissions 0600).
* Automatic reconnection with bounded exponential backoff ensures the system recovers seamlessly from network drops or device reboots.
* Pi 5 continues all computation uninterrupted if Pi 3B goes offline, with a fallback dashboard hosted directly on the Pi 5.

---

## Getting Started

### 1. Build and Test
```bash
# Verify build and test suite
make all

# Run under the Go race detector
make race

# Cross-compile for ARM64 (Pi 5) and ARMv7 (Pi 3B)
make cross
```

### 2. First-Time Setup on Hardware

#### On Raspberry Pi 5 (Research Computer):
```bash
sudo hostnamectl set-hostname piquant-core
sudo make install ROLE=agent
```
The installer initializes the systemd service `piquant-agent.service` and prints the initial access tokens.

#### On Raspberry Pi 3B (Dashboard):
```bash
sudo hostnamectl set-hostname piquant
sudo make install ROLE=dashboard
```
The installer sets memory bounds (`MemoryMax=500M`, `CPUQuota=85%`) to preserve RAM for the operating system and launches `piquant-dashboard.service`.

#### Pairing:
Copy the shared `peer-token` from Pi 5 to Pi 3B:
```bash
# On Pi 5:
sudo piquant secrets get peer-token --dir /etc/piquant

# On Pi 3B:
sudo piquant secrets set peer-token "<TOKEN>" --dir /etc/piquant
sudo systemctl restart piquant-dashboard
```

Both boards automatically discover and authenticate one another.

---

## User Interface & Modes

Open the dashboard in any desktop, tablet, or mobile browser:
* **Primary Dashboard (Pi 3B):** `http://piquant.local:8080`
* **Fallback Dashboard (Pi 5):** `http://piquant-core.local:8080`

### Simple Mode (Default)
Designed for researchers and operators who want immediate results without configuring low-level math:
* Intuitive guided research creation.
* Plain-language status and activity feed ("Historical BTC dataset loaded", "Experiment #142 completed").
* 1-click synthetic dataset generator and model comparisons.

### Advanced Mode
Unlocks granular scientific controls:
* Explicit hyperparameter regularization tuning ($\lambda$).
* Feature set selection (L1, L5, L20, Multi-Level OFI).
* Chronological train/val/test split customization with embargo buffer controls.
* Detailed statistical metrics: MAE, RMSE, Directional Hit Rate %, Spearman rank correlation ($\rho$), and Brier probability calibration score.

---

## Scientific Rigor & Correctness

1. **Deterministic Replay:** Identical raw captures produce identical event sequences and model weights across architectures.
2. **Zero Information Leakage:** Training, validation, and held-out test splits are strictly chronological. An embargo gap ($\ge$ forecast horizon) prevents label overlap.
3. **No Optimization on Test:** Hyperparameter search selects candidate configurations exclusively based on validation split performance. Held-out test performance is evaluated exactly once after selection.
4. **Data Quality Visibility:** Sequence gaps, packet reordering, and normalizer rejections are tracked and displayed.
5. **Crash Recovery:** Interrupted experiments are safely flagged upon process restart and can be resumed with a single click.

---

## Operations & Maintenance

### Restart Services
```bash
# On Pi 5:
sudo systemctl restart piquant-agent

# On Pi 3B:
sudo systemctl restart piquant-dashboard
```

### Run Diagnostics
Run `piquant doctor` on either board for read-only system and hardware health checks:
```bash
piquant doctor
```

### Backup & Restore
* **Configurations & Secrets:** `/etc/piquant/`
* **Datasets & Experiment Results:** `/var/lib/piquant/data/` (or configured data directory)
```bash
# Backup
sudo tar -czvf piquant-backup.tar.gz /etc/piquant /var/lib/piquant/data

# Restore
sudo tar -xzvf piquant-backup.tar.gz -C /
sudo chown -R piquant:piquant /etc/piquant /var/lib/piquant
```
