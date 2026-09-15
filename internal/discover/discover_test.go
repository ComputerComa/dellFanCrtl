package discover

import (
	"testing"

	"dellfanctl/internal/model"
)

func sensor(id string, s model.Sensor) model.Sensor {
	s.ID = id
	return s
}

func TestDiffNoChanges(t *testing.T) {
	old := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-1", model.Sensor{Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"}),
		sensor("ipmi-cpu1-temp", model.Sensor{Source: model.SourceIPMI, IPMIName: "Temp", Occurrence: 1}),
	}}
	fresh := &model.Config{Sensors: []model.Sensor{
		// Same identity (device+device_type / ipmi_name+occurrence), even
		// though discovery would assign these fresh, independent IDs.
		sensor("smart-disk-9", model.Sensor{Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"}),
		sensor("ipmi-cpu1-temp-2", model.Sensor{Source: model.SourceIPMI, IPMIName: "Temp", Occurrence: 1}),
	}}

	res := Diff(old, fresh)
	if res.HasChanges() {
		t.Fatalf("expected no drift, got missing=%v new=%v", res.Missing, res.New)
	}
}

func TestDiffDetectsMissingRequiredDrive(t *testing.T) {
	// The exact shape of the incident that motivated this: a drive slot
	// that used to be readable (e.g. behind a MegaRAID controller) no
	// longer shows up in a fresh probe at all - smartctl either errors or
	// reports no temperature, so discover.Run silently drops it, same as
	// it would for any unreadable device.
	old := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-9", model.Sensor{
			Name: "Disk 9 Temp", Source: model.SourceSMART,
			Device: "/dev/bus/0", DeviceType: "megaraid,12", Required: true,
		}),
	}}
	fresh := &model.Config{Sensors: nil}

	res := Diff(old, fresh)
	if len(res.Missing) != 1 || res.Missing[0].ID != "smart-disk-9" {
		t.Fatalf("expected smart-disk-9 to be reported missing, got %+v", res.Missing)
	}
	if !res.Missing[0].Required {
		t.Fatalf("expected the missing sensor's Required flag to be preserved from the old config")
	}
	if len(res.New) != 0 {
		t.Fatalf("expected no new sensors, got %+v", res.New)
	}
}

func TestDiffDetectsNewDrive(t *testing.T) {
	old := &model.Config{Sensors: nil}
	fresh := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-1", model.Sensor{
			Name: "Disk 1 Temp", Class: model.ClassDisk, Source: model.SourceSMART,
			Device: "/dev/bus/0", DeviceType: "megaraid,3",
		}),
	}}

	res := Diff(old, fresh)
	if len(res.New) != 1 || res.New[0].ID != "smart-disk-1" {
		t.Fatalf("expected smart-disk-1 to be reported new, got %+v", res.New)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("expected no missing sensors, got %+v", res.Missing)
	}
}

func TestDiffSameSlotDifferentDriveIsNotFlagged(t *testing.T) {
	// A like-for-like replacement in the same controller slot: device_type
	// (the physical slot address) is unchanged, so this must NOT show up
	// as either missing or new - it already just works without
	// reconfiguration (see discover.go's doc comment on Sensor.Device).
	old := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-1", model.Sensor{Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"}),
	}}
	fresh := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-1", model.Sensor{Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"}),
	}}

	res := Diff(old, fresh)
	if res.HasChanges() {
		t.Fatalf("expected a same-slot swap to be invisible to Diff, got missing=%v new=%v", res.Missing, res.New)
	}
}
