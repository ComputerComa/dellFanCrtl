package model

import "testing"

func validConfig() Config {
	cfg := Default()
	cfg.Sensors = []Sensor{
		{ID: "cpu1", Name: "CPU1", Class: ClassCPU, Source: SourceIPMI, IPMIName: "Temp", Occurrence: 1},
		{ID: "disk1", Name: "Disk1", Class: ClassDisk, Source: SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"},
	}
	cfg.Groups = []Group{
		{
			Name:        "cpu",
			SensorIDs:   []string{"cpu1"},
			Aggregation: AggMax,
			Enabled:     true,
			Curve:       []CurvePoint{{TempC: 30, FanPct: 20}, {TempC: 80, FanPct: 100}},
		},
	}
	return cfg
}

func TestValidateOK(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidateRejectsUnknownSensorInGroup(t *testing.T) {
	cfg := validConfig()
	cfg.Groups[0].SensorIDs = append(cfg.Groups[0].SensorIDs, "does-not-exist")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown sensor id in group")
	}
}

func TestValidateRejectsDuplicateSensorID(t *testing.T) {
	cfg := validConfig()
	cfg.Sensors = append(cfg.Sensors, cfg.Sensors[0])
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for duplicate sensor id")
	}
}

func TestValidateRejectsUnsortedCurve(t *testing.T) {
	cfg := validConfig()
	cfg.Groups[0].Curve = []CurvePoint{{TempC: 80, FanPct: 100}, {TempC: 30, FanPct: 20}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unsorted curve points")
	}
}

func TestValidateRejectsBadSmoothing(t *testing.T) {
	cfg := validConfig()
	cfg.Smoothing.MinFanPct = 50
	cfg.Smoothing.MaxFanPct = 20
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when max_fan_pct <= min_fan_pct")
	}
}

func TestValidateRejectsMissingIPMIName(t *testing.T) {
	cfg := validConfig()
	cfg.Sensors[0].IPMIName = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for ipmi sensor missing ipmi_name")
	}
}

func TestValidateIgnoresDisabledGroupSensorRefs(t *testing.T) {
	cfg := validConfig()
	cfg.Groups = append(cfg.Groups, Group{
		Name:      "disabled-group",
		SensorIDs: []string{"nonexistent"},
		Enabled:   false,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled group should not be validated strictly, got: %v", err)
	}
}
