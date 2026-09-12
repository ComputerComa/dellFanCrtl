// Package curve implements piecewise-linear fan curve interpolation.
package curve

import "dellfanctl/internal/model"

// Interpolate returns the fan percent for temp given a set of curve points.
// Points need not be pre-sorted here (config.Validate enforces sorted input
// at load time, but this stays correct either way by finding the bracketing
// pair directly). Below the lowest point's temp, that point's percent is
// returned; above the highest, the highest's percent is returned.
func Interpolate(points []model.CurvePoint, temp float64) float64 {
	if len(points) == 0 {
		return 0
	}
	lo, hi := points[0], points[0]
	for _, p := range points {
		if p.TempC < lo.TempC {
			lo = p
		}
		if p.TempC > hi.TempC {
			hi = p
		}
	}
	if temp <= lo.TempC {
		return lo.FanPct
	}
	if temp >= hi.TempC {
		return hi.FanPct
	}
	// Find the bracketing pair with the smallest span containing temp.
	var left, right *model.CurvePoint
	for i := range points {
		p := &points[i]
		if p.TempC <= temp && (left == nil || p.TempC > left.TempC) {
			left = p
		}
		if p.TempC >= temp && (right == nil || p.TempC < right.TempC) {
			right = p
		}
	}
	if left == nil || right == nil {
		return lo.FanPct
	}
	if left.TempC == right.TempC {
		// temp lands exactly on a known point.
		return left.FanPct
	}
	frac := (temp - left.TempC) / (right.TempC - left.TempC)
	return left.FanPct + frac*(right.FanPct-left.FanPct)
}
