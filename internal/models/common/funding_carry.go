package common

import (
	"math"
)

// FundingCarryTracker monitors spot and perpetual swap prices, computes mark-index premium,
// 8-hour funding rates with exchange damping clamp, time-weighted EWMA predicted funding,
// annualized carry APR %, and funding-aware reservation price skews.
//
// All hot paths execute in 0 B/op and 0 allocs/op.
type FundingCarryTracker struct {
	AlphaFunding    float64 // EWMA decay factor for predicted funding (default: 0.05)
	InterestRate    float64 // Baseline interest rate per 8h window (default: 0.0001 = 0.01%)
	ClampBand       float64 // Exchange damping clamp band (default: 0.0005 = 0.05%)
	RebateWeight    float64 // Incentive weight to harvest funding rebate (default: 0.75)
	InventoryWeight float64 // Penalty weight for holding inventory on paying side (default: 1.25)

	LastSpotPrice    float64
	LastPerpPrice    float64
	LastTimestampNs  int64
	CurrentPremium   float64 // P_premium = (P_perp - P_spot) / P_spot
	InstantFunding   float64 // Instantaneous funding rate per 8h
	PredictedFunding float64 // Time-weighted EWMA predicted funding rate per 8h
	UpdateCount      int64
}

// NewFundingCarryTracker initializes a FundingCarryTracker with specified EWMA alpha
// and interest rate baseline.
func NewFundingCarryTracker(alpha, interestRate float64) *FundingCarryTracker {
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.05
	}
	if interestRate <= 0 {
		interestRate = 0.0001 // 0.01% standard interest rate
	}
	return &FundingCarryTracker{
		AlphaFunding:     alpha,
		InterestRate:     interestRate,
		ClampBand:        0.0005, // 0.05%
		RebateWeight:     0.75,
		InventoryWeight:  1.25,
		PredictedFunding: interestRate,
	}
}

// UpdatePrices updates spot and perp prices and calculates the damped funding rate.
// Formula:
//
//	P_premium = (P_perp - P_spot) / P_spot
//	clamp(I - P_premium, -0.05%, +0.05%)
//	F_instant = P_premium + clamp(I - P_premium, -0.05%, +0.05%)
//	F_pred = (1 - alpha)*F_pred + alpha*F_instant
//
// Hot path: 0 B/op, 0 allocs/op.
func (f *FundingCarryTracker) UpdatePrices(perpPrice, spotPrice float64, timestampNs int64) float64 {
	if perpPrice <= 0 || spotPrice <= 0 {
		return f.InstantFunding
	}

	premium := (perpPrice - spotPrice) / spotPrice
	f.LastSpotPrice = spotPrice
	f.LastPerpPrice = perpPrice
	f.LastTimestampNs = timestampNs
	f.CurrentPremium = premium

	// Damped clamping: clamp(I - P_premium, -0.05%, +0.05%)
	diff := f.InterestRate - premium
	clampedDiff := diff
	if clampedDiff > f.ClampBand {
		clampedDiff = f.ClampBand
	} else if clampedDiff < -f.ClampBand {
		clampedDiff = -f.ClampBand
	}

	instantFunding := premium + clampedDiff
	f.InstantFunding = instantFunding

	if f.UpdateCount == 0 {
		f.PredictedFunding = instantFunding
	} else {
		alpha := f.AlphaFunding
		if alpha <= 0 || alpha > 1.0 {
			alpha = 0.05
		}
		f.PredictedFunding = (1.0-alpha)*f.PredictedFunding + alpha*instantFunding
	}
	f.UpdateCount++

	return instantFunding
}

// PredictedFundingBps returns the predicted 8-hour funding rate in basis points (1 bps = 0.01%).
// Hot path: 0 B/op, 0 allocs/op.
func (f *FundingCarryTracker) PredictedFundingBps() float64 {
	return f.PredictedFunding * 10000.0
}

// AnnualizedAPR returns the annualized funding carry yield percentage APR.
// 8-hour funding settlements occur 3 times daily (3 * 365 = 1095 periods per year).
// Hot path: 0 B/op, 0 allocs/op.
func (f *FundingCarryTracker) AnnualizedAPR() float64 {
	// 1095 funding intervals per 365-day year
	return f.PredictedFunding * 1095.0 * 100.0
}

// FundingSkewBps computes the funding-aware reservation price skew in basis points:
// skews maker quotes to harvest positive funding rebates while penalizing inventory held
// on the paying side as settlement approaches.
//
// Parameters:
//   - inventory: net base units held (positive = long, negative = short)
//   - timeToSettlementSec: seconds remaining until the next 8h funding settlement in [0, 28800]
//
// Hot path: 0 B/op, 0 allocs/op.
func (f *FundingCarryTracker) FundingSkewBps(inventory, timeToSettlementSec float64) float64 {
	fundingBps := f.PredictedFundingBps()
	if math.Abs(fundingBps) < 1e-4 {
		return 0.0
	}

	// 8 hours = 28,800 seconds
	const windowSec = 28800.0
	if timeToSettlementSec < 0 {
		timeToSettlementSec = 0
	} else if timeToSettlementSec > windowSec {
		timeToSettlementSec = windowSec
	}

	// Urgency factor \tau(t) \in [0.10, 1.0]: increases as settlement approaches (t -> 0)
	timeUrgency := 0.10 + 0.90*(windowSec-timeToSettlementSec)/windowSec

	// Base rebate incentive:
	// If fundingBps > 0 (longs pay shorts): we skew downward (sell/short) to earn rebate.
	// If fundingBps < 0 (shorts pay longs): we skew upward (buy/long) to earn rebate.
	rebateSkew := -math.Copysign(1.0, fundingBps) * math.Min(math.Abs(fundingBps)*f.RebateWeight, 2.5) * timeUrgency

	// Inventory penalty for holding on paying side:
	// If fundingBps > 0 and inventory > 0: long is paying, penalize by pushing reservation price lower.
	// If fundingBps < 0 and inventory < 0: short is paying, penalize by pushing reservation price higher.
	var invPenalty float64
	payingExposure := inventory * fundingBps
	if payingExposure > 0 {
		// Held on paying side -> accelerate offloading
		invPenalty = -math.Copysign(1.0, inventory) * math.Min(math.Abs(inventory)*0.5*math.Abs(fundingBps)*f.InventoryWeight, 5.0) * timeUrgency
	} else if payingExposure < 0 {
		// Held on receiving side -> support holding up to reasonable cap
		invPenalty = math.Copysign(1.0, inventory) * math.Min(math.Abs(inventory)*0.2*math.Abs(fundingBps), 1.5) * timeUrgency
	}

	totalSkew := rebateSkew + invPenalty
	// Clamp total skew to [-8.0, +8.0] bps to prevent quote blowout
	if totalSkew > 8.0 {
		totalSkew = 8.0
	} else if totalSkew < -8.0 {
		totalSkew = -8.0
	}

	return totalSkew
}

// FundingCarryModel wraps FundingCarryTracker and base regression into the ContinuousModel interface.
type FundingCarryModel struct {
	BaseRidge Ridge
	Tracker   *FundingCarryTracker
	fitted    bool
}

// NewFundingCarryModel constructs an unfitted FundingCarryModel.
func NewFundingCarryModel(lambda float64) *FundingCarryModel {
	if lambda <= 0 {
		lambda = 1e-3
	}
	return &FundingCarryModel{
		BaseRidge: Ridge{Lam: lambda},
		Tracker:   NewFundingCarryTracker(0.05, 0.0001),
	}
}

// Fit trains the base Ridge model on the provided training samples.
func (m *FundingCarryModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	useIdx := idx
	if len(useIdx) == 0 {
		useIdx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := m.BaseRidge.Fit(samples, useIdx); err != nil {
		return err
	}
	m.fitted = true
	return nil
}

// Predict calculates forward alpha forecast combining order book signals with funding carry rate.
// Hot path: 0 B/op, 0 allocs/op.
func (m *FundingCarryModel) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if m.fitted {
		baseAlpha = m.BaseRidge.Predict(x)
	} else {
		// Default lightweight feature-driven proxy
		baseAlpha = 0.35*x[0]*x[4] + 0.25*x[5] + 0.20*x[11]
	}

	// Funding carry edge contribution:
	// Basis / funding pull exerts predictable drift on perp mid price towards index
	fundingBps := m.Tracker.PredictedFundingBps()
	carryDrift := -0.15 * fundingBps // Perp converges toward spot as settlement approaches

	return baseAlpha + carryDrift
}
