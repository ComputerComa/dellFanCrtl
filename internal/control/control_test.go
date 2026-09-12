package control

import (
	"testing"

	"dellfanctl/internal/model"
)

func testConfig() *model.Config {
	c := model.Default()
	cfg := &c
	cfg.Smoothing.MaxStepUpPct = 10
	cfg.Smoothing.MaxStepDownPct = 5
	cfg.Smoothing.RampDownHoldTicks = 3
	cfg.Smoothing.MinFanPct = 10
	cfg.Smoothing.MaxFanPct = 100
	cfg.Sensors = []model.Sensor{
		{ID: "cpu1", Name: "CPU1", Class: model.ClassCPU, Source: model.SourceIPMI, IPMIName: "Temp", Occurrence: 1, Required: true, CritC: 90},
		{ID: "fan1", Name: "Fan1", Class: model.ClassFanRPM, Source: model.SourceIPMI, IPMIName: "Fan1 RPM", Required: true, MinRPM: 500},
	}
	cfg.Groups = []model.Group{
		{Name: "cpu", SensorIDs: []string{"cpu1"}, Aggregation: model.AggMax, Enabled: true,
			Curve: []model.CurvePoint{{TempC: 30, FanPct: 20}, {TempC: 80, FanPct: 100}}},
	}
	return cfg
}

func TestRampTowardFirstApplicationJumpsDirectly(t *testing.T) {
	c := New(testConfig(), nil, true)
	got := c.rampToward(75)
	if got != 75 {
		t.Errorf("first application should jump directly to desired, got %v", got)
	}
}

func TestRampTowardStepsUpGradually(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.commandedPct = 20
	// Desired far above current: should only move by MaxStepUpPct (10).
	got := c.rampToward(90)
	if got != 30 {
		t.Errorf("expected ramp-limited step to 30, got %v", got)
	}
}

func TestRampTowardHoldsBeforeSteppingDown(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.commandedPct = 50
	// First two ticks asking for a lower speed should hold steady
	// (RampDownHoldTicks=3); the third should finally step down.
	if got := c.rampToward(20); got != 50 {
		t.Errorf("tick 1: expected hold at 50, got %v", got)
	}
	if got := c.rampToward(20); got != 50 {
		t.Errorf("tick 2: expected hold at 50, got %v", got)
	}
	got := c.rampToward(20)
	if got != 45 { // 50 - MaxStepDownPct(5)
		t.Errorf("tick 3: expected step down to 45, got %v", got)
	}
}

func TestRampTowardRespectsMinMax(t *testing.T) {
	c := New(testConfig(), nil, true)
	if got := c.rampToward(5); got != 10 {
		t.Errorf("expected clamp to min 10, got %v", got)
	}
	c2 := New(testConfig(), nil, true)
	if got := c2.rampToward(150); got != 100 {
		t.Errorf("expected clamp to max 100, got %v", got)
	}
}

func TestSafetyTripOnCriticalTemp(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.recordReading(c.states["cpu1"], 95) // above CritC=90
	c.recordReading(c.states["fan1"], 5000)
	if reason := c.safetyTrip(); reason == "" {
		t.Fatal("expected safety trip for over-temp CPU")
	}
}

func TestSafetyTripOnFanFailure(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.recordReading(c.states["cpu1"], 40)
	c.recordReading(c.states["fan1"], 100) // below MinRPM=500
	if reason := c.safetyTrip(); reason == "" {
		t.Fatal("expected safety trip for failed fan")
	}
}

func TestSafetyTripClearWhenNormal(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.recordReading(c.states["cpu1"], 40)
	c.recordReading(c.states["fan1"], 5000)
	if reason := c.safetyTrip(); reason != "" {
		t.Fatalf("expected no safety trip, got: %q", reason)
	}
}

func TestSafetyTripOnStaleRequiredSensor(t *testing.T) {
	c := New(testConfig(), nil, true)
	c.recordReading(c.states["fan1"], 5000)
	st := c.states["cpu1"]
	for i := 0; i <= c.cfg.Safety.MaxConsecutiveErrors; i++ {
		c.markStale(st)
	}
	if reason := c.safetyTrip(); reason == "" {
		t.Fatal("expected safety trip for stale required sensor")
	}
}

func TestAggregateModes(t *testing.T) {
	cfg := testConfig()
	cfg.Sensors = append(cfg.Sensors, model.Sensor{ID: "cpu2", Class: model.ClassCPU, Source: model.SourceIPMI, IPMIName: "Temp", Occurrence: 2})
	c := New(cfg, nil, true)
	c.recordReading(c.states["cpu1"], 40)
	c.recordReading(c.states["cpu2"], 60)

	max, ok := c.aggregate(model.Group{SensorIDs: []string{"cpu1", "cpu2"}, Aggregation: model.AggMax})
	if !ok || max != 60 {
		t.Errorf("max aggregate = %v, ok=%v, want 60", max, ok)
	}
	min, ok := c.aggregate(model.Group{SensorIDs: []string{"cpu1", "cpu2"}, Aggregation: model.AggMin})
	if !ok || min != 40 {
		t.Errorf("min aggregate = %v, ok=%v, want 40", min, ok)
	}
	avg, ok := c.aggregate(model.Group{SensorIDs: []string{"cpu1", "cpu2"}, Aggregation: model.AggAvg})
	if !ok || avg != 50 {
		t.Errorf("avg aggregate = %v, ok=%v, want 50", avg, ok)
	}
}

func TestEMASmoothsSpike(t *testing.T) {
	cfg := testConfig()
	cfg.Smoothing.EMAAlpha = 0.3
	c := New(cfg, nil, true)
	st := c.states["cpu1"]
	c.recordReading(st, 40)
	c.recordReading(st, 40)
	c.recordReading(st, 40)
	// A brief single-tick spike should only partially move the EMA.
	c.recordReading(st, 100)
	if st.ema >= 100 || st.ema <= 40 {
		t.Errorf("expected EMA to be damped between 40 and 100 after one spike, got %v", st.ema)
	}
	if st.ema > 60 {
		t.Errorf("expected EMA to still be well below the spike value, got %v", st.ema)
	}
}
