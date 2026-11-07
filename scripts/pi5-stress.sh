#!/usr/bin/env bash
# scripts/pi5-stress.sh — Raspberry Pi 5 Architecture Micro-Latency Stress Harness
#
# Simulates the Raspberry Pi 5 hardware profile (4x Cortex-A76 cores, 8GB RAM)
# on x86_64 development workstations using taskset, GOMAXPROCS=4, GOMEMLIMIT=2GiB,
# and evaluates the full tick-to-decision path against the <1.0ms latency budget.
#
# Invariants:
#   - Full path: ring -> book -> features -> models -> paper engine
#   - Replays 1x (100 ev/s), 10x (1,000 ev/s), and burst (>20k ev/s)
#   - Pass criteria: p99 x 4.0 (Pi factor) < 1.0ms, zero ring drops at 10x.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

export PATH="/home/everett/.local/go/bin:${PATH}"
cd "${REPO_ROOT}"

EVENTS="${EVENTS:-5000}"
MODE="${1:-all}"

echo "=========================================================================="
echo " [PiQuant] Running Raspberry Pi 5 Architecture Micro-Latency Stress Test"
echo " Configuration: taskset -c 0-3, GOMAXPROCS=4, GOMEMLIMIT=2GiB"
echo " Target Events: ${EVENTS} per scenario | Mode: ${MODE}"
echo "=========================================================================="

if [ ! -f "bin/piquant" ]; then
    echo "Building native binary bin/piquant..."
    CGO_ENABLED=0 go build -trimpath -o bin/piquant ./cmd/piquant
fi

# Execute harness under simulated Pi 5 CPU & memory constraints
taskset -c 0-3 env GOMAXPROCS=4 GOMEMLIMIT=2GiB ./bin/piquant bench --events "${EVENTS}" --mode "${MODE}"
