package mqttpub

import (
	"testing"

	"dellfanctl/internal/model"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"MyNAS":        "mynas",
		"my nas!!":     "my_nas_",
		"CPU1 Temp":    "cpu1_temp",
		"":             "x",
		"already_fine": "already_fine",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassInfoTemperatureClasses(t *testing.T) {
	for _, c := range []model.SensorClass{model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard, model.ClassDisk, model.ClassMemory} {
		unit, deviceClass, _, isTemp := classInfo(c)
		if unit != "°C" || deviceClass != "temperature" || !isTemp {
			t.Errorf("classInfo(%v) = unit=%q deviceClass=%q isTemp=%v, want °C/temperature/true", c, unit, deviceClass, isTemp)
		}
	}
}

func TestClassInfoFanRPM(t *testing.T) {
	unit, deviceClass, category, isTemp := classInfo(model.ClassFanRPM)
	if unit != "RPM" || deviceClass != "" || category != "diagnostic" || isTemp {
		t.Errorf("classInfo(fan_rpm) = %q/%q/%q/%v, want RPM//diagnostic/false", unit, deviceClass, category, isTemp)
	}
}

func TestTitleCase(t *testing.T) {
	if got := titleCase("cpu"); got != "Cpu" {
		t.Errorf("titleCase(cpu) = %q, want Cpu", got)
	}
	if got := titleCase(""); got != "" {
		t.Errorf("titleCase(\"\") = %q, want empty", got)
	}
}

func TestNewDerivesBaseTopicFromNodeID(t *testing.T) {
	cfg := model.MQTT{NodeID: "MyNAS"}
	p := New(cfg, nil)
	if p.node != "mynas" {
		t.Errorf("node = %q, want mynas", p.node)
	}
	if p.base != "dellfanctl/mynas" {
		t.Errorf("base = %q, want dellfanctl/mynas", p.base)
	}
}

func TestNewRespectsExplicitBaseTopic(t *testing.T) {
	cfg := model.MQTT{NodeID: "nas1", BaseTopic: "custom/topic"}
	p := New(cfg, nil)
	if p.base != "custom/topic" {
		t.Errorf("base = %q, want custom/topic", p.base)
	}
}
