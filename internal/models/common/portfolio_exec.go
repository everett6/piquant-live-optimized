package common

import "math"

// AlmgrenChrissSchedule computes the analytical hyperbolic (sinh/cosh) Almgren–Chriss
// child-order execution trajectory n_k (for k = 0..slices-1) that minimizes expected
// implementation shortfall plus variance penalty:
//
//	x_k = X * sinh(kappa * (T - k*tau)) / sinh(kappa * T),   n_k = x_k - x_{k+1}
//	where kappa = sqrt(lambda * sigma^2 / eta).
//
// Returned slice sums strictly to totalQty.
func AlmgrenChrissSchedule(totalQty float64, slices int, volatility float64, tempImpactEta float64, riskAversionLambda float64) []float64 {
	if slices <= 0 {
		return nil
	}
	out := make([]float64, slices)
	return AlmgrenChrissScheduleInto(out, totalQty, slices, volatility, tempImpactEta, riskAversionLambda)
}

// AlmgrenChrissScheduleInto writes the Almgren–Chriss child-order schedule into dst
// without allocating when cap(dst) >= slices.
func AlmgrenChrissScheduleInto(dst []float64, totalQty float64, slices int, volatility float64, tempImpactEta float64, riskAversionLambda float64) []float64 {
	if slices <= 0 {
		return nil
	}
	if cap(dst) < slices {
		dst = make([]float64, slices)
	} else {
		dst = dst[:slices]
	}
	if totalQty == 0 {
		for i := range dst {
			dst[i] = 0
		}
		return dst
	}
	if slices == 1 {
		dst[0] = totalQty
		return dst
	}

	sigma := math.Max(1e-6, volatility)
	eta := math.Max(1e-6, tempImpactEta)
	lam := math.Max(0, riskAversionLambda)

	// Urgency parameter kappa = sqrt(lambda * sigma^2 / eta)
	kappa := math.Sqrt(lam * sigma * sigma / eta)
	bigT := float64(slices)
	denom := math.Sinh(kappa * bigT)

	// Risk-neutral limit (kappa -> 0): uniform TWAP schedule
	if kappa*bigT < 1e-4 || math.IsInf(denom, 0) || denom <= 1e-12 {
		uniform := totalQty / bigT
		var sum float64
		for k := 0; k < slices-1; k++ {
			dst[k] = uniform
			sum += uniform
		}
		dst[slices-1] = totalQty - sum
		return dst
	}

	prevRem := totalQty
	var sumAllocated float64
	for k := 0; k < slices-1; k++ {
		remNext := totalQty * math.Sinh(kappa*(bigT-float64(k+1))) / denom
		nk := prevRem - remNext
		if totalQty > 0 && nk < 0 {
			nk = 0
		} else if totalQty < 0 && nk > 0 {
			nk = 0
		}
		dst[k] = nk
		sumAllocated += nk
		prevRem = remNext
	}
	dst[slices-1] = totalQty - sumAllocated
	return dst
}

// MultiAssetKellyAllocate computes fractional multi-asset Kelly weights:
//
//	w* = fraction * Sigma^{-1} * mu
//
// across the 3-asset universe [BTCUSDT, ETHUSDT, BTC-ETH Stat-Arb] entirely on the stack
// (0 allocs/op) with correlation-aware inverse covariance and gross leverage capping.
func MultiAssetKellyAllocate(expectedEdgesBps [3]float64, volsBps [3]float64, corrBTCETH float64, fraction float64, maxLeverage float64) [3]float64 {
	var w [3]float64
	if fraction <= 0 {
		fraction = 0.5 // Half-Kelly default
	}
	if maxLeverage <= 0 {
		maxLeverage = 1.0
	}

	// Clamp BTC-ETH correlation away from singularity [-0.92, 0.92]
	rho := corrBTCETH
	if rho > 0.92 {
		rho = 0.92
	} else if rho < -0.92 {
		rho = -0.92
	}

	v0 := math.Max(1.0, volsBps[0])
	v1 := math.Max(1.0, volsBps[1])
	v2 := math.Max(1.0, volsBps[2])

	// Convert bps to decimal return units (1 bps = 1e-4) or work in normalized variance units:
	// Using normalized return/volatility ratio (mu_i / sigma_i^2) scaled so realistic bps edges
	// map to institutional portfolio weights.
	s0Sq := v0 * v0
	s1Sq := v1 * v1
	s2Sq := v2 * v2

	// 2x2 block inverse for correlated directional legs (BTCUSDT, ETHUSDT):
	// Sigma_01 = [[s0^2, rho*s0*s1], [rho*s0*s1, s1^2]]
	oneMinusRhoSq := 1.0 - rho*rho
	invDet := 1.0 / (s0Sq * s1Sq * oneMinusRhoSq)

	mu0 := expectedEdgesBps[0]
	mu1 := expectedEdgesBps[1]
	mu2 := expectedEdgesBps[2]

	// Scale factor converts bps/bps^2 into unit leverage weights
	const scale = 25.0
	raw0 := fraction * scale * invDet * (s1Sq*mu0 - rho*v0*v1*mu1)
	raw1 := fraction * scale * invDet * (-rho*v0*v1*mu0 + s0Sq*mu1)
	// Market-neutral BTC-ETH Stat-Arb leg is orthogonal to directional beta
	raw2 := fraction * scale * (mu2 / s2Sq)

	w[0] = raw0
	w[1] = raw1
	w[2] = raw2

	// Enforce gross leverage cap: |w[0]| + |w[1]| + |w[2]| <= maxLeverage
	gross := math.Abs(w[0]) + math.Abs(w[1]) + math.Abs(w[2])
	if gross > maxLeverage && gross > 1e-12 {
		shrink := maxLeverage / gross
		w[0] *= shrink
		w[1] *= shrink
		w[2] *= shrink
	}

	return w
}
