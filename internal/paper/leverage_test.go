package paper

import (
	"testing"
)

func TestDynamicLeverageController_CalmRegime(t *testing.T) {
	cfg := DefaultDynamicLeverageConfig()
	ctrl := NewDynamicLeverageController(cfg)

	// In calm regime: vol=15bps, vpin=0.25, hawkes=0.50, dd=0.10%
	st := ctrl.Compute(15.0, 0.25, 0.50, 0.10, false)

	if st.EffectiveLeverage < 2.0 || st.EffectiveLeverage > 3.0 {
		t.Errorf("expected calm leverage ~2.5x, got %.2fx", st.EffectiveLeverage)
	}
	if st.RegimeName != "CALM_MAKER" {
		t.Errorf("expected CALM_MAKER, got %s", st.RegimeName)
	}

	// Compute order size for $1,000 equity on BTC at $65,000
	btcSize := ctrl.ComputeOrderSize(1000.0, st.EffectiveLeverage, 65000.0, 2)
	// Notional should be ~$1,250 -> ~0.0192 BTC
	notional := btcSize * 65000.0
	if notional < 1000.0 || notional > 1500.0 {
		t.Errorf("expected order notional ~$1250, got $%.2f (size=%.4f)", notional, btcSize)
	}
}

func TestDynamicLeverageController_ToxicFlowAndCascade(t *testing.T) {
	cfg := DefaultDynamicLeverageConfig()
	ctrl := NewDynamicLeverageController(cfg)

	// Toxic regime: vpin=0.65
	stToxic := ctrl.Compute(15.0, 0.65, 0.50, 0.10, false)
	if stToxic.EffectiveLeverage > 1.5 {
		t.Errorf("expected toxic leverage to de-leverage <= 1.5x, got %.2fx", stToxic.EffectiveLeverage)
	}
	if stToxic.RegimeName != "TOXIC_DEFENSE" {
		t.Errorf("expected TOXIC_DEFENSE regime, got %s", stToxic.RegimeName)
	}

	// Cascade regime: hawkes=0.95
	stCascade := ctrl.Compute(15.0, 0.20, 0.95, 0.10, false)
	if stCascade.EffectiveLeverage > 1.0 {
		t.Errorf("expected cascade leverage <= 1.0x, got %.2fx", stCascade.EffectiveLeverage)
	}
	if stCascade.RegimeName != "CASCADE_ALERT" {
		t.Errorf("expected CASCADE_ALERT regime, got %s", stCascade.RegimeName)
	}

	// Halted regime: circuit breaker tripped
	stHalted := ctrl.Compute(15.0, 0.20, 0.50, 2.10, true)
	if stHalted.EffectiveLeverage != 0.0 {
		t.Errorf("expected 0.0x leverage when halted, got %.2fx", stHalted.EffectiveLeverage)
	}
}
