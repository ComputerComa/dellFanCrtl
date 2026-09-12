package classify

import (
	"testing"

	"dellfanctl/internal/model"
)

func TestIPMISensor(t *testing.T) {
	cases := []struct {
		name, unit, entity string
		want               model.SensorClass
	}{
		{"Inlet Temp", "degrees C", "7.1", model.ClassInlet},
		{"Exhaust Temp", "degrees C", "7.1", model.ClassExhaust},
		{"Temp", "degrees C", "3.1", model.ClassCPU}, // generic name, processor entity
		{"Temp", "degrees C", "3.2", model.ClassCPU},
		{"Temp", "degrees C", "7.1", model.ClassBoard}, // generic name, non-processor entity
		{"Fan1 RPM", "RPM", "7.1", model.ClassFanRPM},
		{"CPU Usage", "percent", "7.1", model.ClassUtilization},
		{"Voltage 1", "Volts", "10.1", model.ClassPower},
		{"Current 1", "Amps", "10.1", model.ClassPower},
		{"DIMM A1 Temp", "degrees C", "", model.ClassMemory},
		{"PSU1 Status", "", "", model.ClassPSU},
	}
	for _, c := range cases {
		got := IPMISensor(c.name, c.unit, c.entity)
		if got != c.want {
			t.Errorf("IPMISensor(%q,%q,%q) = %q, want %q", c.name, c.unit, c.entity, got, c.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	if got := DisplayName("Temp", model.ClassCPU, 1); got != "CPU1 Temp" {
		t.Errorf("got %q, want CPU1 Temp", got)
	}
	if got := DisplayName("Temp", model.ClassCPU, 2); got != "CPU2 Temp" {
		t.Errorf("got %q, want CPU2 Temp", got)
	}
	if got := DisplayName("Inlet Temp", model.ClassInlet, 1); got != "Inlet Temp" {
		t.Errorf("got %q, want unchanged name", got)
	}
}
