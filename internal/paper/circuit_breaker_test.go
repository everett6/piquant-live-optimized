package paper

import (
	"testing"
	"time"
)

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now()

	// Initial state: Normal
	st, reason := cb.OnTick(100000.0, 1.0, 0.25, now)
	if st != StateNormal {
		t.Fatalf("expected StateNormal, got %s (%s)", st, reason)
	}
	if cb.State.SizingMultiplier() != 1.0 {
		t.Errorf("expected 1.0 sizing multiplier in StateNormal, got %f", cb.State.SizingMultiplier())
	}
	if !cb.State.CanEnterNewPositions() {
		t.Errorf("expected CanEnterNewPositions = true in StateNormal")
	}

	// Warning Drawdown (2.6% >= 2.5%)
	st, reason = cb.OnTick(97400.0, 1.0, 0.25, now.Add(time.Second))
	if st != StateWarning {
		t.Fatalf("expected StateWarning on 2.6%% drawdown, got %s (%s)", st, reason)
	}
	if cb.State.SizingMultiplier() != 0.5 {
		t.Errorf("expected 0.5 sizing multiplier in StateWarning, got %f", cb.State.SizingMultiplier())
	}

	// Tripped Drawdown (3.6% >= 3.5%)
	st, reason = cb.OnTick(96400.0, 1.0, 0.25, now.Add(2*time.Second))
	if st != StateTripped {
		t.Fatalf("expected StateTripped on 3.6%% drawdown, got %s (%s)", st, reason)
	}
	if cb.State.SizingMultiplier() != 0.0 {
		t.Errorf("expected 0.0 sizing multiplier in StateTripped, got %f", cb.State.SizingMultiplier())
	}
	if cb.State.CanEnterNewPositions() {
		t.Errorf("expected CanEnterNewPositions = false in StateTripped")
	}
	if cb.State.CanReduceOnly() {
		t.Errorf("expected CanReduceOnly = false in StateTripped")
	}
}

func TestCircuitBreaker_InventoryCap(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())

	// Inventory exceeds cap (2.5 > 2.0)
	st, reason := cb.OnFill(10.0, 2.5)
	if st != StateReduceOnly {
		t.Fatalf("expected StateReduceOnly when inventory exceeds cap, got %s (%s)", st, reason)
	}
	if cb.State.CanEnterNewPositions() {
		t.Errorf("expected CanEnterNewPositions = false in StateReduceOnly")
	}
	if !cb.State.CanReduceOnly() {
		t.Errorf("expected CanReduceOnly = true in StateReduceOnly")
	}

	// Bring inventory back under cap
	st, _ = cb.OnFill(10.0, 1.2)
	// Process normal tick
	st, _ = cb.OnTick(100000.0, 1.0, 0.25, time.Now())
	if st != StateNormal {
		t.Fatalf("expected return to StateNormal after inventory reduced, got %s", st)
	}
}

func TestCircuitBreaker_ConsecutiveLosses(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())

	// Incur 4 losses (limit is 5)
	for i := 0; i < 4; i++ {
		st, _ := cb.OnFill(-15.0, 0.5)
		if st != StateNormal {
			t.Fatalf("expected StateNormal on loss %d, got %s", i+1, st)
		}
	}

	// 5th consecutive loss triggers warning
	st, reason := cb.OnFill(-20.0, 0.5)
	if st != StateWarning {
		t.Fatalf("expected StateWarning on 5 consecutive losses, got %s (%s)", st, reason)
	}

	// A winning trade resets the counter
	st, _ = cb.OnFill(50.0, 0.5)
	if cb.ConsecutiveLosses != 0 {
		t.Fatalf("expected consecutive losses reset to 0, got %d", cb.ConsecutiveLosses)
	}
}

func TestCircuitBreaker_SpreadBlowout(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now()

	// Spread > 50 bps triggers StateTripped
	st, reason := cb.OnTick(100000.0, 55.0, 0.30, now)
	if st != StateTripped {
		t.Fatalf("expected StateTripped on spread blowout, got %s (%s)", st, reason)
	}
}

func TestCircuitBreaker_VPINToxicity(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now()

	// VPIN >= 0.70 triggers StateReduceOnly
	st, reason := cb.OnTick(100000.0, 1.5, 0.75, now)
	if st != StateReduceOnly {
		t.Fatalf("expected StateReduceOnly on toxic VPIN, got %s (%s)", st, reason)
	}
}

func TestCircuitBreaker_AutoCooldown(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cfg.CooldownDuration = 3 * time.Minute
	cb := NewCircuitBreaker(cfg)
	start := time.Now()

	// Trip circuit breaker with spread blowout
	st, _ := cb.OnTick(100000.0, 60.0, 0.20, start)
	if st != StateTripped {
		t.Fatalf("expected StateTripped, got %s", st)
	}

	// 1 minute in, spread has normalized but cooldown (3m) is not complete
	st, _ = cb.OnTick(100000.0, 1.0, 0.20, start.Add(1*time.Minute))
	if st != StateTripped {
		t.Fatalf("expected to remain StateTripped during cooldown, got %s", st)
	}

	// 2 minutes in, another spread spike resets/extends cooldown
	st, _ = cb.OnTick(100000.0, 55.0, 0.20, start.Add(2*time.Minute))
	if st != StateTripped {
		t.Fatalf("expected to remain StateTripped, got %s", st)
	}

	// 4 minutes from start (2 min after spike), cooldown still active
	st, _ = cb.OnTick(100000.0, 1.0, 0.20, start.Add(4*time.Minute))
	if st != StateTripped {
		t.Fatalf("expected to remain StateTripped, got %s", st)
	}

	// 5.5 minutes from start (3.5 min after spike with stable pricing): should recover to Normal
	st, reason := cb.OnTick(100000.0, 1.0, 0.20, start.Add(5*time.Minute+30*time.Second))
	if st != StateNormal {
		t.Fatalf("expected recovery to StateNormal after cooldown, got %s (%s)", st, reason)
	}
}

func BenchmarkCircuitBreaker_OnTick(b *testing.B) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cb.OnTick(100000.0, 1.25, 0.35, now)
	}
}

func BenchmarkCircuitBreaker_OnFill(b *testing.B) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cb.OnFill(12.50, 0.75)
	}
}
