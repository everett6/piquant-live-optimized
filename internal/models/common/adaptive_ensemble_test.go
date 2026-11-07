package common

import (
	"math"
	"testing"
)

func TestAdaptiveMetaEnsembleRegimes(t *testing.T) {
	// 1. Check regime classification thresholds
	rLow := ClassifyMarketRegime(0.8, 0.4, 0.3)
	if rLow != RegimeLowVolRange {
		t.Fatalf("expected RegimeLowVolRange, got %v", rLow)
	}

	rTrend := ClassifyMarketRegime(2.2, 1.2, 0.5)
	if rTrend != RegimeTrending {
		t.Fatalf("expected RegimeTrending, got %v", rTrend)
	}

	rCascade1 := ClassifyMarketRegime(1.5, 0.5, 0.88)
	if rCascade1 != RegimeHighVolCascade {
		t.Fatalf("expected RegimeHighVolCascade from branching ratio, got %v", rCascade1)
	}

	rCascade2 := ClassifyMarketRegime(4.5, 0.5, 0.4)
	if rCascade2 != RegimeHighVolCascade {
		t.Fatalf("expected RegimeHighVolCascade from volatility, got %v", rCascade2)
	}

	rCascade3 := ClassifyMarketRegime(1.0, 2.5, 0.4)
	if rCascade3 != RegimeHighVolCascade {
		t.Fatalf("expected RegimeHighVolCascade from jump ratio, got %v", rCascade3)
	}
}

func TestAdaptiveMetaEnsembleWeightsAndLearning(t *testing.T) {
	ens := NewAdaptiveMetaEnsemble(1e-3, 0.98, 0.20)

	// Check initial uniform weights
	expectedInit := 1.0 / float64(NumMetaExperts)
	for r := 0; r < NumMicroRegimes; r++ {
		for i := 0; i < NumMetaExperts; i++ {
			if math.Abs(ens.Weights[r][i]-expectedInit) > 1e-6 {
				t.Fatalf("regime %d expert %d initial weight = %f, want %f", r, i, ens.Weights[r][i], expectedInit)
			}
		}
	}

	// Initial top expert
	id, name, w := ens.TopExpert(RegimeLowVolRange)
	if id != 0 || name != MetaExpertNames[0] || math.Abs(w-expectedInit) > 1e-6 {
		t.Fatalf("unexpected initial top expert: id=%d, name=%s, weight=%f", id, name, w)
	}

	// Reward ExpertLeadLag (index 2) in RegimeTrending
	var rewards [NumMetaExperts]float64
	rewards[ExpertLeadLag] = 5.0
	ens.UpdateEdge(RegimeTrending, rewards)

	idTrend, nameTrend, wTrend := ens.TopExpert(RegimeTrending)
	if idTrend != ExpertLeadLag {
		t.Fatalf("expected TopExpert in RegimeTrending to be ExpertLeadLag (2), got %d (%s)", idTrend, nameTrend)
	}
	if wTrend <= expectedInit {
		t.Fatalf("expected rewarded expert weight to increase from %f, got %f", expectedInit, wTrend)
	}

	// Verify weights sum to 1.0 (simplex)
	var sumW float64
	for i := 0; i < NumMetaExperts; i++ {
		sumW += ens.Weights[RegimeTrending][i]
	}
	if math.Abs(sumW-1.0) > 1e-6 {
		t.Fatalf("weights do not sum to 1.0: %f", sumW)
	}

	// Predict should be deterministic and non-zero on active features
	var x [FeatureCount]float64
	x[0] = 0.4
	x[3] = 0.1
	x[4] = 1.0
	x[5] = 1.2
	x[8] = 0.8
	x[10] = 2.0
	x[11] = 0.5
	x[15] = 0.6

	ens.UpdateSensors(2.0, 0.5, 0.3)
	if ens.Regime() != RegimeTrending {
		t.Fatalf("expected RegimeTrending, got %v", ens.Regime())
	}

	pred := ens.Predict(&x)
	if math.IsNaN(pred) || math.IsInf(pred, 0) || pred == 0 {
		t.Fatalf("invalid prediction: %f", pred)
	}
}

func BenchmarkAdaptiveMetaEnsemblePredict(b *testing.B) {
	ens := NewAdaptiveMetaEnsemble(1e-3, 0.98, 0.15)
	var x [FeatureCount]float64
	x[0] = 0.3
	x[4] = 1.0
	x[5] = 1.5
	x[8] = 0.7
	x[10] = 1.2
	x[11] = 0.4
	x[13] = 0.2
	x[14] = 3.0

	b.ResetTimer()
	b.ReportAllocs()
	var dummy float64
	for i := 0; i < b.N; i++ {
		dummy += ens.Predict(&x)
	}
	if math.IsNaN(dummy) {
		b.Fatal("unexpected NaN")
	}
}

func BenchmarkAdaptiveMetaEnsembleUpdateEdge(b *testing.B) {
	ens := NewAdaptiveMetaEnsemble(1e-3, 0.98, 0.15)
	rewards := [NumMetaExperts]float64{0.5, 1.2, 0.8, -0.2, 0.1, 0.4}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ens.UpdateEdge(RegimeTrending, rewards)
	}
}

func BenchmarkAdaptiveMetaEnsembleTopExpert(b *testing.B) {
	ens := NewAdaptiveMetaEnsemble(1e-3, 0.98, 0.15)

	b.ResetTimer()
	b.ReportAllocs()
	var topID uint8
	var topName string
	var topW float64
	for i := 0; i < b.N; i++ {
		topID, topName, topW = ens.TopExpert(RegimeLowVolRange)
	}
	if topW == 0 || topName == "" || topID > NumMetaExperts {
		b.Fatal("unexpected TopExpert result")
	}
}
