package paper

// Watchdog State constants for the Dry-Run Hardware & Feed Watchdog.
const (
	WatchdogNominal             = "NOMINAL"
	WatchdogDegradedPassiveOnly = "DEGRADED_PASSIVE_ONLY"
	WatchdogHaltedStaleFeed     = "HALTED_STALE_FEED"
)

// WatchdogReport is a stack-allocated health snapshot produced by DryRunWatchdog.Check
// in 0 B/op and 0 allocs/op.
type WatchdogReport struct {
	State           string  `json:"state"`            // "NOMINAL", "DEGRADED_PASSIVE_ONLY", or "HALTED_STALE_FEED"
	StaleMs         float64 `json:"stale_ms"`         // Elapsed time since last market feed event (ms)
	RingLoadPct     float64 `json:"ring_load_pct"`    // SPSC ring buffer occupancy percentage [0, 100]
	ThermalThrottle bool    `json:"thermal_throttle"` // True if RPi 5 core temp >= MaxTempC or firmware throttled
	Backpressure    bool    `json:"backpressure"`     // True if SPSC ring occupancy exceeds MaxRingLoadPct (80%)
	StaleFeed       bool    `json:"stale_feed"`       // True if feed silence exceeds MaxStaleNs
	Action          string  `json:"action"`           // Deterministic execution action directive
}

// DryRunWatchdog monitors market feed freshness, SPSC ring buffer backpressure, and
// Raspberry Pi 5 thermal/voltage throttling on the hot execution path.
type DryRunWatchdog struct {
	MaxStaleNs     int64   // Maximum tolerated feed silence in nanoseconds (default: 2.5s = 2_500_000_000)
	MaxRingLoadPct float64 // SPSC ring backpressure threshold percentage (default: 80.0%)
	MaxTempC       float64 // Raspberry Pi 5 thermal throttle threshold in Celsius (default: 78.0 C)
}

// NewDryRunWatchdog returns a calibrated DryRunWatchdog with institutional Raspberry Pi 5 defaults.
func NewDryRunWatchdog() DryRunWatchdog {
	return DryRunWatchdog{
		MaxStaleNs:     2_500_000_000, // 2.5 seconds
		MaxRingLoadPct: 80.0,          // 80% SPSC ring occupancy
		MaxTempC:       78.0,          // 78.0 C thermal ceiling before BCM2712 soft limit
	}
}

// Check evaluates market stream staleness, SPSC ring backpressure, and Raspberry Pi 5
// thermal/under-voltage throttling in 0 B/op and 0 allocs/op.
func (w DryRunWatchdog) Check(nowNs, lastEventNs int64, ringLen, ringCap int, tempC float64, throttled bool) WatchdogReport {
	maxStale := w.MaxStaleNs
	if maxStale <= 0 {
		maxStale = 2_500_000_000
	}
	maxRingPct := w.MaxRingLoadPct
	if maxRingPct <= 0 {
		maxRingPct = 80.0
	}
	maxTemp := w.MaxTempC
	if maxTemp <= 0 {
		maxTemp = 78.0
	}

	var staleNs int64
	if lastEventNs > 0 && nowNs > lastEventNs {
		staleNs = nowNs - lastEventNs
	} else if lastEventNs <= 0 {
		staleNs = maxStale + 1
	}
	staleMs := float64(staleNs) / 1e6

	var ringLoadPct float64
	if ringCap > 0 && ringLen > 0 {
		ringLoadPct = (float64(ringLen) / float64(ringCap)) * 100.0
		if ringLoadPct > 100.0 {
			ringLoadPct = 100.0
		}
	}

	isStale := staleNs > maxStale
	isBackpressured := ringLoadPct > maxRingPct
	isThermal := tempC >= maxTemp || throttled

	if isStale {
		return WatchdogReport{
			State:           WatchdogHaltedStaleFeed,
			StaleMs:         staleMs,
			RingLoadPct:     ringLoadPct,
			ThermalThrottle: isThermal,
			Backpressure:    isBackpressured,
			StaleFeed:       true,
			Action:          "CANCEL_ALL_QUOTES_HALT_EXECUTION",
		}
	}

	if isBackpressured || isThermal {
		action := "WIDEN_SPREADS_PASSIVE_MAKER_ONLY"
		if isBackpressured && isThermal {
			action = "SHED_TAKER_LOAD_AND_THROTTLE_INFERENCE"
		} else if isBackpressured {
			action = "DRAIN_SPSC_RING_PASSIVE_ONLY"
		}
		return WatchdogReport{
			State:           WatchdogDegradedPassiveOnly,
			StaleMs:         staleMs,
			RingLoadPct:     ringLoadPct,
			ThermalThrottle: isThermal,
			Backpressure:    isBackpressured,
			StaleFeed:       false,
			Action:          action,
		}
	}

	return WatchdogReport{
		State:           WatchdogNominal,
		StaleMs:         staleMs,
		RingLoadPct:     ringLoadPct,
		ThermalThrottle: false,
		Backpressure:    false,
		StaleFeed:       false,
		Action:          "FULL_DUPLEX_SMART_ROUTING",
	}
}
