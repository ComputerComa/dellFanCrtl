package curve

import (
	"testing"

	"dellfanctl/internal/model"
)

func TestInterpolate(t *testing.T) {
	pts := []model.CurvePoint{
		{TempC: 30, FanPct: 20},
		{TempC: 50, FanPct: 40},
		{TempC: 70, FanPct: 100},
	}
	cases := []struct {
		temp float64
		want float64
	}{
		{10, 20},  // below range -> clamp to first
		{30, 20},  // exactly first point
		{40, 30},  // midpoint of first segment
		{50, 40},  // exactly middle point
		{60, 70},  // midpoint of second segment
		{70, 100}, // exactly last point
		{90, 100}, // above range -> clamp to last
	}
	for _, c := range cases {
		got := Interpolate(pts, c.temp)
		if got != c.want {
			t.Errorf("Interpolate(%v) = %v, want %v", c.temp, got, c.want)
		}
	}
}

func TestInterpolateSinglePoint(t *testing.T) {
	pts := []model.CurvePoint{{TempC: 40, FanPct: 50}}
	if got := Interpolate(pts, 10); got != 50 {
		t.Errorf("got %v, want 50", got)
	}
	if got := Interpolate(pts, 90); got != 50 {
		t.Errorf("got %v, want 50", got)
	}
}

func TestInterpolateEmpty(t *testing.T) {
	if got := Interpolate(nil, 50); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}
