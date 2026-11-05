package common

import (
	"math"
)

// CarteaJaimungalMM implements the Cartea–Jaimungal Hamilton-Jacobi-Bellman (HJB)
// optimal execution and market-making model with:
//   - Running inventory penalty: \phi q_t^2
//   - Terminal inventory penalty: \alpha (q_T - q_{target})^2
//   - Market impact / adverse selection parameter: \rho \cdot OFI
//
// Equations:
//
//	r(s, q, t) = s - q * (2\phi (T - t) + \alpha) - \rho \cdot OFI
//	\delta^{a*}, \delta^{b*} = \frac{1}{\gamma} \ln(1 + \frac{\gamma}{\kappa}) + \frac{2q \pm 1}{2} \sqrt{\frac{\sigma^2 \gamma}{2 \kappa} + \frac{\phi}{\kappa}}
//
// All methods execute in 0 B/op and 0 allocs/op.
// Layout is exactly 64 bytes (8 * float64), fitting entirely into a single Cortex-A76 L1 cache line.
type CarteaJaimungalMM struct {
	Gamma       float64 // Absolute risk aversion \gamma
	Kappa       float64 // Order book liquidity intensity parameter \kappa
	Sigma       float64 // Volatility \sigma
	Phi         float64 // Running inventory penalty \phi
	Alpha       float64 // Terminal inventory penalty \alpha
	Rho         float64 // Adverse selection / market impact parameter \rho
	HorizonT    float64 // Total execution horizon T (seconds)
	MakerFeeBps float64 // Exchange fee / rebate adjustment
}

// NewCarteaJaimungalMM constructs a calibrated Cartea–Jaimungal market maker.
func NewCarteaJaimungalMM(gamma, kappa, sigma, phi, alpha, rho, horizonT float64) *CarteaJaimungalMM {
	if gamma <= 0 {
		gamma = 0.05
	}
	if kappa <= 0 {
		kappa = 1.5
	}
	if sigma <= 0 {
		sigma = 1.0
	}
	if phi < 0 {
		phi = 0.001
	}
	if alpha < 0 {
		alpha = 0.01
	}
	if horizonT <= 0 {
		horizonT = 60.0
	}
	return &CarteaJaimungalMM{
		Gamma:    gamma,
		Kappa:    kappa,
		Sigma:    sigma,
		Phi:      phi,
		Alpha:    alpha,
		Rho:      rho,
		HorizonT: horizonT,
	}
}

// ComputeQuotes calculates the optimal bid price, ask price, and reservation price
// under closed-form Cartea–Jaimungal HJB formulas.
//
// Zero allocations (0 B/op, 0 allocs/op).
func (m *CarteaJaimungalMM) ComputeQuotes(midPrice, inventory, targetInventory, timeRemaining, ofi float64) (bidPrice, askPrice, reservationPrice float64) {
	if midPrice <= 0 {
		return 0, 0, 0
	}
	if timeRemaining < 0 {
		timeRemaining = 0
	}

	// Inventory net of target: \tilde{q} = q - q_target
	netInv := inventory - targetInventory

	// Reservation price:
	// r(s, q, t) = s - (q - q_target) * (2\phi (T - t) + \alpha) - \rho \cdot OFI
	invPenalty := netInv * (2.0*m.Phi*timeRemaining + m.Alpha)
	reservationPrice = midPrice - invPenalty - m.Rho*ofi

	// Asymptotic / finite-horizon half-spread terms:
	// spreadBase = (1 / gamma) * ln(1 + gamma / kappa)
	spreadBase := (1.0 / m.Gamma) * math.Log(1.0+m.Gamma/m.Kappa)

	// Spread inventory skew term:
	// rad = sqrt( (sigma^2 * gamma) / (2 * kappa) + phi / kappa )
	rad := math.Sqrt((m.Sigma*m.Sigma*m.Gamma)/(2.0*m.Kappa) + m.Phi/m.Kappa)

	// Half spreads under Cartea-Jaimungal HJB:
	// delta^{a*} = spreadBase + ((-2*netInv + 1) / 2) * rad
	// delta^{b*} = spreadBase + ((2*netInv + 1) / 2) * rad
	deltaA := spreadBase + ((-2.0*netInv+1.0)/2.0)*rad
	deltaB := spreadBase + ((2.0*netInv+1.0)/2.0)*rad

	// Guard against non-positive half spreads
	if deltaA < 0.01 {
		deltaA = 0.01
	}
	if deltaB < 0.01 {
		deltaB = 0.01
	}

	askPrice = reservationPrice + deltaA
	bidPrice = reservationPrice - deltaB

	// Cross-guard: ensure ask > bid
	if askPrice <= bidPrice {
		half := spreadBase
		if half < 0.01 {
			half = 0.01
		}
		askPrice = reservationPrice + half
		bidPrice = reservationPrice - half
	}

	return bidPrice, askPrice, reservationPrice
}

// Fit satisfies ContinuousModel interface.
func (m *CarteaJaimungalMM) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	// Estimate empirical volatility and liquidity intensity kappa
	var sumSpread, sumTrades float64
	for i := range samples {
		sp := samples[i].X[4] // spread_bps
		if sp > 0 {
			sumSpread += sp
		}
		sumTrades += math.Max(0, samples[i].X[14]) // trade_cnt_1s
	}
	n := float64(len(samples))
	if n > 0 {
		meanSpread := sumSpread / n
		meanTrades := sumTrades / n
		if meanSpread > 0.05 {
			m.Kappa = math.Max(0.4, math.Min(8.0, (1.0+meanTrades*0.2)/meanSpread))
		}
	}
	return nil
}

// Predict provides a continuous reservation price shift (in bps from mid) for evaluation.
func (m *CarteaJaimungalMM) Predict(x *[FeatureCount]float64) float64 {
	mid := 10000.0 // normalized mid
	// x[4] is spread_bps, x[5] is ofi_1s, x[10] is vol_30s_bps
	ofi := x[5]
	vol := math.Max(0.1, x[10])
	m.Sigma = vol * 0.1 // update instantaneous sigma
	_, _, r := m.ComputeQuotes(mid, 0, 0, m.HorizonT, ofi)
	return (r - mid) / mid * 1e4
}
