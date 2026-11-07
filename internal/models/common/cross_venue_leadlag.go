package common

import (
	"math"
)

// LeadLagSignal captures instantaneous cross-venue price discovery dynamics,
// Hasbrouck Information Share, and forecasted follower price drift.
type LeadLagSignal struct {
	LeaderVenue       string  `json:"leader_venue"`
	FollowerVenue     string  `json:"follower_venue"`
	LeadLagSpreadBps  float64 `json:"lead_lag_spread_bps"`
	PredictedDriftBps float64 `json:"predicted_drift_bps"`
	Confidence        float64 `json:"confidence"`
	UrgencyBps        float64 `json:"urgency_bps"`
	InfoShareA        float64 `json:"info_share_a"`
	InfoShareB        float64 `json:"info_share_b"`
	DeltaPriceA       float64 `json:"delta_price_a"`
	DeltaPriceB       float64 `json:"delta_price_b"`
	TickLeadOffsetUs  int64   `json:"tick_lead_offset_us"`
}

// CrossVenueLeadLagTracker tracks synchronous timestamps and price innovations across
// Venue A (e.g. Binance) and Venue B (e.g. Bybit or Spot).
//
// Computes high-frequency Hasbrouck (1995) Information Share and microsecond tick-lead
// correlation to detect when the lead venue moves before the follower book updates,
// calculating expected follower price drift and emitting LeadLagSignal with 0 B/op and 0 allocs/op.
type CrossVenueLeadLagTracker struct {
	VenueA string
	VenueB string

	// Current prices & timestamps
	PriceA float64
	PriceB float64
	TsANs  int64
	TsBNs  int64

	// Innovations
	PrevPriceA float64
	PrevPriceB float64
	DeltaA     float64
	DeltaB     float64

	// Online EWMA statistics for Hasbrouck Information Share (decay alpha = 0.05)
	varA  float64
	varB  float64
	covAB float64
	decay float64
	count uint64

	// Cached latest signal
	latestSignal LeadLagSignal

	// Baseline continuous predictor for feature scoring in experiment runner
	BaseRidge Ridge
	fitted    bool
	AlphaWt   float64
}

// NewCrossVenueLeadLagTracker creates an instance of CrossVenueLeadLagTracker.
func NewCrossVenueLeadLagTracker(venueA, venueB string) *CrossVenueLeadLagTracker {
	if venueA == "" {
		venueA = "Binance"
	}
	if venueB == "" {
		venueB = "Bybit"
	}
	return &CrossVenueLeadLagTracker{
		VenueA:  venueA,
		VenueB:  venueB,
		decay:   0.05,
		AlphaWt: 0.60,
		BaseRidge: Ridge{
			Lam: 1e-3,
		},
		latestSignal: LeadLagSignal{
			LeaderVenue:   venueA,
			FollowerVenue: venueB,
			InfoShareA:    0.50,
			InfoShareB:    0.50,
			Confidence:    0.50,
		},
	}
}

// UpdatePrices updates synchronous prices and timestamps across both venues.
// Operates in 0 B/op and 0 allocs/op.
func (t *CrossVenueLeadLagTracker) UpdatePrices(priceA, priceB float64, tsANs, tsBNs int64) LeadLagSignal {
	if priceA <= 0 || priceB <= 0 {
		return t.latestSignal
	}

	if t.PriceA > 0 && t.PriceB > 0 {
		t.DeltaA = priceA - t.PriceA
		t.DeltaB = priceB - t.PriceB
	} else {
		t.DeltaA = 0
		t.DeltaB = 0
	}

	t.PrevPriceA = t.PriceA
	t.PrevPriceB = t.PriceB
	t.PriceA = priceA
	t.PriceB = priceB
	t.TsANs = tsANs
	t.TsBNs = tsBNs

	return t.Evaluate()
}

// UpdateVenueTick updates a single venue's price print and re-evaluates the lead-lag state.
// Operates in 0 B/op and 0 allocs/op.
func (t *CrossVenueLeadLagTracker) UpdateVenueTick(venue string, price float64, tsNs int64) LeadLagSignal {
	if price <= 0 {
		return t.latestSignal
	}

	if venue == t.VenueA {
		if t.PriceA > 0 {
			t.DeltaA = price - t.PriceA
		}
		t.PrevPriceA = t.PriceA
		t.PriceA = price
		t.TsANs = tsNs
	} else {
		if t.PriceB > 0 {
			t.DeltaB = price - t.PriceB
		}
		t.PrevPriceB = t.PriceB
		t.PriceB = price
		t.TsBNs = tsNs
	}

	return t.Evaluate()
}

// Evaluate computes high-frequency Hasbrouck Information Share, tick lead offset,
// and expected follower price drift.
// Operates in 0 B/op and 0 allocs/op.
func (t *CrossVenueLeadLagTracker) Evaluate() LeadLagSignal {
	if t.PriceA <= 0 || t.PriceB <= 0 {
		return t.latestSignal
	}

	// LeadLagSpread_bps = ((P_A - P_B) / P_B) * 10,000
	spreadBps := ((t.PriceA - t.PriceB) / t.PriceB) * 10000.0

	// Microsecond tick lead offset (positive if Venue A updated earlier/first)
	var tickLeadUs int64
	if t.TsANs > 0 && t.TsBNs > 0 {
		tickLeadUs = (t.TsBNs - t.TsANs) / 1000
	}

	// Online EWMA update for Hasbrouck Information Share
	dA := t.DeltaA
	dB := t.DeltaB
	if t.count == 0 {
		t.varA = dA * dA
		t.varB = dB * dB
		t.covAB = dA * dB
	} else {
		a := t.decay
		t.varA = (1.0-a)*t.varA + a*(dA*dA)
		t.varB = (1.0-a)*t.varB + a*(dB*dB)
		t.covAB = (1.0-a)*t.covAB + a*(dA*dB)
	}
	t.count++

	// Hasbrouck (1995) Information Share estimation from covariance matrix:
	// Var(Delta P) bounds under lower/upper triangularization
	totVar := t.varA + t.varB
	infoShareA := 0.50
	infoShareB := 0.50

	if totVar > 1e-12 {
		stdA := math.Sqrt(t.varA)
		stdB := math.Sqrt(t.varB)
		corr := 0.0
		if stdA > 1e-6 && stdB > 1e-6 {
			corr = t.covAB / (stdA * stdB)
			if corr > 0.999 {
				corr = 0.999
			} else if corr < -0.999 {
				corr = -0.999
			}
		}

		denom := t.varA - 2.0*corr*stdA*stdB + t.varB
		if denom > 1e-12 {
			// Hasbrouck upper and lower bounds
			isA_lower := math.Pow(stdA-corr*stdB, 2) / denom
			isA_upper := t.varA / denom
			infoShareA = (isA_lower + isA_upper) * 0.5
		} else {
			infoShareA = t.varA / totVar
		}

		if infoShareA < 0.01 {
			infoShareA = 0.01
		} else if infoShareA > 0.99 {
			infoShareA = 0.99
		}
		infoShareB = 1.0 - infoShareA
	}

	// Leader / Follower determination
	var leaderVenue, followerVenue string
	var predictedDriftBps, confidence float64

	if infoShareA >= infoShareB {
		leaderVenue = t.VenueA
		followerVenue = t.VenueB
		confidence = math.Min(1.0, 0.5+math.Abs(infoShareA-0.5)*1.5)

		// Follower venue B expected to drift towards venue A to close the spread
		predictedDriftBps = spreadBps * infoShareA
	} else {
		leaderVenue = t.VenueB
		followerVenue = t.VenueA
		confidence = math.Min(1.0, 0.5+math.Abs(infoShareB-0.5)*1.5)

		// Follower venue A expected to drift towards venue B
		predictedDriftBps = -spreadBps * infoShareB
	}

	urgencyBps := math.Abs(predictedDriftBps) * confidence

	sig := LeadLagSignal{
		LeaderVenue:       leaderVenue,
		FollowerVenue:     followerVenue,
		LeadLagSpreadBps:  spreadBps,
		PredictedDriftBps: predictedDriftBps,
		Confidence:        confidence,
		UrgencyBps:        urgencyBps,
		InfoShareA:        infoShareA,
		InfoShareB:        infoShareB,
		DeltaPriceA:       t.DeltaA,
		DeltaPriceB:       t.DeltaB,
		TickLeadOffsetUs:  tickLeadUs,
	}

	t.latestSignal = sig
	return sig
}

// Signal returns the most recent LeadLagSignal.
func (t *CrossVenueLeadLagTracker) Signal() LeadLagSignal {
	return t.latestSignal
}

// Reset clears the price innovations and statistics.
func (t *CrossVenueLeadLagTracker) Reset() {
	t.PriceA = 0
	t.PriceB = 0
	t.TsANs = 0
	t.TsBNs = 0
	t.PrevPriceA = 0
	t.PrevPriceB = 0
	t.DeltaA = 0
	t.DeltaB = 0
	t.varA = 0
	t.varB = 0
	t.covAB = 0
	t.count = 0
	t.latestSignal = LeadLagSignal{
		LeaderVenue:   t.VenueA,
		FollowerVenue: t.VenueB,
		InfoShareA:    0.50,
		InfoShareB:    0.50,
		Confidence:    0.50,
	}
}

// Fit trains the underlying Ridge predictor for continuous model evaluation.
func (t *CrossVenueLeadLagTracker) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	if len(idx) == 0 {
		idx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := t.BaseRidge.Fit(samples, idx); err != nil {
		return err
	}
	t.fitted = true
	return nil
}

// Predict forecasts forward mid price return conditioned on order book features and lead-lag drift.
// Operates in 0 B/op and 0 allocs/op.
func (t *CrossVenueLeadLagTracker) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if t.fitted {
		baseAlpha = t.BaseRidge.Predict(x)
	}

	// Microstructure cross-flow signal
	microDev := x[3]
	spreadBps := math.Max(0.1, x[4])
	ofiFast := x[5]  // ofi_1s
	ofiSlow := x[6]  // ofi_5s
	retFast := x[11] // ret_1s_bps

	crossSignal := microDev*spreadBps*0.8 + 0.3*ofiFast + 0.15*ofiSlow + 0.2*retFast
	return baseAlpha + t.AlphaWt*crossSignal
}

// CrossVenueLeadLagModel wraps CrossVenueLeadLagTracker to implement ContinuousModel.
type CrossVenueLeadLagModel = CrossVenueLeadLagTracker

// NewCrossVenueLeadLagModel creates a CrossVenueLeadLagModel instance.
func NewCrossVenueLeadLagModel(lambda float64, venueA, venueB string) *CrossVenueLeadLagModel {
	tr := NewCrossVenueLeadLagTracker(venueA, venueB)
	tr.BaseRidge.Lam = lambda
	return tr
}
