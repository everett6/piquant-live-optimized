package common

import (
	"math"
	"math/rand/v2"
	"testing"
)

// synth builds samples where y is a known linear function of two features plus
// noise, so a correct fit can be checked against the generating coefficients.
func synth(n int, seed uint64, coef [2]float64, noise float64) []Sample {
	rng := rand.New(rand.NewPCG(seed, 7))
	out := make([]Sample, n)
	for i := range out {
		x1 := rng.Float64()*2 - 1
		x2 := rng.Float64()*10 - 5
		y := coef[0]*x1 + coef[1]*x2 + noise*(rng.Float64()*2-1)
		out[i] = Sample{X: [FeatureCount]float64{x1, x2}, Y: y}
	}
	return out
}

func TestRidgeRecoversKnownCoefficients(t *testing.T) {
	samples := synth(4000, 1, [2]float64{3, -0.5}, 0.01)
	r := NewRidge(1e-8)
	if err := r.Fit(samples, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Weights[0]-3) > 0.02 || math.Abs(r.Weights[1]+0.5) > 0.02 {
		t.Fatalf("weights = %v %v, want ~3 and ~-0.5", r.Weights[0], r.Weights[1])
	}
	// An unused feature must have zero weight, not a leftover value.
	if r.Weights[5] != 0 {
		t.Fatalf("unused feature weight = %v", r.Weights[5])
	}
	var x [FeatureCount]float64
	x[0], x[1] = 0.5, 2
	if got, want := r.Predict(&x), 3*0.5-0.5*2; math.Abs(got-want) > 0.05 {
		t.Fatalf("Predict = %v, want ~%v", got, want)
	}
}

// The penalty must shrink coefficients monotonically and never explode.
func TestRidgePenaltyShrinks(t *testing.T) {
	samples := synth(1000, 2, [2]float64{2, 1}, 1)
	var prev float64 = math.Inf(1)
	for _, lam := range []float64{1e-6, 1e-2, 1, 100} {
		r := NewRidge(lam)
		if err := r.Fit(samples, []int{0, 1}); err != nil {
			t.Fatal(err)
		}
		norm := r.Weights[0]*r.Weights[0] + r.Weights[1]*r.Weights[1]
		if norm > prev {
			t.Fatalf("penalty %g increased the coefficient norm (%g > %g)", lam, norm, prev)
		}
		prev = norm
	}
}

func TestRidgeEdgeCases(t *testing.T) {
	r := NewRidge(0.1)
	if err := r.Fit(nil, []int{0}); err != ErrNotFitted {
		t.Fatalf("empty fit err = %v", err)
	}
	// A constant feature must not divide by zero; its weight stays 0.
	samples := make([]Sample, 50)
	for i := range samples {
		samples[i] = Sample{X: [FeatureCount]float64{5}, Y: float64(i % 2)}
	}
	if err := r.Fit(samples, []int{0}); err != nil {
		t.Fatal(err)
	}
	if r.Weights[0] != 0 || math.IsNaN(r.Bias) {
		t.Fatalf("constant feature: weight=%v bias=%v", r.Weights[0], r.Bias)
	}
	// Non-finite inputs produce non-finite output rather than a silent zero.
	var x [FeatureCount]float64
	x[0] = math.NaN()
	if Finite(r.Predict(&x)) {
		t.Fatal("NaN input produced a finite prediction")
	}
}

func TestLogisticSeparatesClasses(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 9))
	samples := make([]Sample, 3000)
	for i := range samples {
		x := rng.Float64()*2 - 1
		var y float64
		if x > 0 {
			y = 1
		}
		if rng.Float64() < 0.1 { // 10% label noise
			y = 1 - y
		}
		samples[i] = Sample{X: [FeatureCount]float64{x}, Y: y}
	}
	l := NewLogistic(1e-3)
	if err := l.Fit(samples, []int{0}); err != nil {
		t.Fatal(err)
	}
	var pos, neg [FeatureCount]float64
	pos[0], neg[0] = 0.8, -0.8
	if l.Prob(&pos) <= 0.7 || l.Prob(&neg) >= 0.3 {
		t.Fatalf("P(+0.8)=%v P(-0.8)=%v", l.Prob(&pos), l.Prob(&neg))
	}
	if l.Iters == 0 || l.Iters > maxIter {
		t.Fatalf("iterations = %d", l.Iters)
	}
	// Deterministic: refitting gives identical weights.
	l2 := NewLogistic(1e-3)
	l2.Fit(samples, []int{0})
	if l.Weights != l2.Weights || l.Bias != l2.Bias {
		t.Fatal("logistic fit is not deterministic")
	}
}

func TestLogisticEdgeCases(t *testing.T) {
	l := NewLogistic(1e-3)
	// Not fitted: returns the neutral probability rather than a confident 0.
	var x [FeatureCount]float64
	if p := l.Prob(&x); p != 0.5 {
		t.Fatalf("unfitted Prob = %v", p)
	}
	// All-one-class data must fit without dividing by zero.
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{X: [FeatureCount]float64{float64(i)}, Y: 1}
	}
	if err := l.Fit(samples, []int{0}); err != nil {
		t.Fatalf("single-class fit: %v", err)
	}
	if p := l.Prob(&x); !Finite(p) || p < 0.5 {
		t.Fatalf("single-class Prob = %v", p)
	}
	// Empty input is an error, not a zero model.
	if err := l.Fit(nil, []int{0}); err == nil {
		t.Fatal("empty fit accepted")
	}
	// Non-binary labels are skipped; with no valid labels the fit fails.
	if err := l.Fit([]Sample{{Y: 0.5}}, []int{0}); err == nil {
		t.Fatal("non-binary label accepted")
	}
}

func TestHorizonParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Horizon
	}{{"30s", H30s}, {"1m", H1m}, {"60", H1m}, {"5m", H5m}, {"300s", H5m}} {
		got, err := ParseHorizon(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseHorizon(%q) = %v, %v", tc.in, got, err)
		}
	}
	if _, err := ParseHorizon("2h"); err == nil {
		t.Fatal("bad horizon accepted")
	}
	if H30s.Seconds() != 30 || H1m.Seconds() != 60 || H5m.Seconds() != 300 {
		t.Fatal("horizon durations")
	}
	if H30s.String() != "30s" || H1m.String() != "1m" || H5m.String() != "5m" {
		t.Fatal("horizon names")
	}
}

func TestBps(t *testing.T) {
	if got := Bps(100, 101); math.Abs(got-100) > 1e-9 {
		t.Fatalf("Bps = %v", got)
	}
	if Bps(0, 1) != 0 {
		t.Fatal("Bps from a zero price")
	}
	if ValidFeature(-1) || ValidFeature(FeatureCount) || !ValidFeature(0) {
		t.Fatal("ValidFeature")
	}
}

func BenchmarkRidgeFit(b *testing.B) {
	samples := synth(2000, 4, [2]float64{1, 1}, 1)
	idx := []int{0, 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewRidge(1e-3)
		if err := r.Fit(samples, idx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLogisticFit(b *testing.B) {
	samples := synth(2000, 5, [2]float64{1, 1}, 1)
	for i := range samples {
		if samples[i].Y > 0 {
			samples[i].Y = 1
		} else {
			samples[i].Y = 0
		}
	}
	idx := []int{0, 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewLogistic(1e-3)
		if err := l.Fit(samples, idx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDot16(b *testing.B) {
	var w, x [FeatureCount]float64
	for i := 0; i < FeatureCount; i++ {
		w[i] = float64(i) * 0.1
		x[i] = float64(16-i) * 0.2
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += Dot16(&w, &x)
	}
	if math.IsNaN(sink) {
		b.Fatal("unexpected NaN")
	}
}

func BenchmarkRidgePredict(b *testing.B) {
	r := NewRidge(1e-3)
	for i := 0; i < FeatureCount; i++ {
		r.Weights[i] = float64(i) * 0.05
	}
	r.Bias = 1.23
	var x [FeatureCount]float64
	for i := 0; i < FeatureCount; i++ {
		x[i] = float64(i) * 0.1
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += r.Predict(&x)
	}
	if math.IsNaN(sink) {
		b.Fatal("unexpected NaN")
	}
}
