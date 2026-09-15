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

func TestMergePreservesMQTT(t *testing.T) {
	old := &model.Config{MQTT: model.MQTT{Enabled: true, Broker: "tcp://mqtt.example.lan:1883", Username: "dellfanctl"}}
	fresh := &model.Config{MQTT: model.MQTT{}} // what Run would actually produce: untouched zero value

	rep := Merge(old, fresh)
	if !rep.MQTTPreserved {
		t.Fatalf("expected MQTTPreserved to be true")
	}
	if fresh.MQTT != old.MQTT {
		t.Fatalf("expected fresh.MQTT to be overwritten with old.MQTT, got %+v", fresh.MQTT)
	}
}

func TestMergeKeepsCurveByGroupName(t *testing.T) {
	handTunedCurve := []model.CurvePoint{{TempC: 20, FanPct: 10}, {TempC: 60, FanPct: 100}}
	old := &model.Config{Groups: []model.Group{
		{Name: "disks", SensorIDs: []string{"smart-disk-1"}, Aggregation: model.AggAvg, Enabled: false, Curve: handTunedCurve},
	}}
	// A fresh run: same group name, but a newly (re)computed sensor_ids
	// list and discovery's stock default curve/enabled.
	fresh := &model.Config{Groups: []model.Group{
		{Name: "disks", SensorIDs: []string{"smart-disk-1", "smart-disk-2"}, Aggregation: model.AggAvg, Enabled: true,
			Curve: []model.CurvePoint{{TempC: 25, FanPct: 20}, {TempC: 55, FanPct: 100}}},
	}}

	rep := Merge(old, fresh)
	if len(rep.CurvesPreserved) != 1 || rep.CurvesPreserved[0] != "disks" {
		t.Fatalf("expected CurvesPreserved=[disks], got %v", rep.CurvesPreserved)
	}
	g := fresh.Groups[0]
	if g.Enabled {
		t.Errorf("expected Enabled=false to be kept from old config")
	}
	if len(g.Curve) != 2 || g.Curve[0].FanPct != 10 {
		t.Errorf("expected the hand-tuned curve to be kept, got %+v", g.Curve)
	}
	// SensorIDs must NOT be pinned to the old, stale list - they need to
	// reflect what's actually there now.
	if len(g.SensorIDs) != 2 {
		t.Errorf("expected the freshly computed sensor_ids to be kept (2 disks), got %v", g.SensorIDs)
	}
}

func TestMergeCarriesOverCustomGroup(t *testing.T) {
	old := &model.Config{Groups: []model.Group{
		{Name: "nvme-only", SensorIDs: []string{"smart-disk-3"}, Aggregation: model.AggMax, Enabled: true,
			Curve: []model.CurvePoint{{TempC: 40, FanPct: 50}}},
	}}
	fresh := &model.Config{Groups: []model.Group{
		{Name: "disks", SensorIDs: []string{"smart-disk-1"}, Aggregation: model.AggAvg, Enabled: true},
	}}

	rep := Merge(old, fresh)
	if len(rep.GroupsCarriedOver) != 1 || rep.GroupsCarriedOver[0] != "nvme-only" {
		t.Fatalf("expected GroupsCarriedOver=[nvme-only], got %v", rep.GroupsCarriedOver)
	}
	if len(fresh.Groups) != 2 {
		t.Fatalf("expected the custom group to be appended alongside the regenerated one, got %+v", fresh.Groups)
	}
}

func TestMergeReappliesPerSensorThresholdsByIdentity(t *testing.T) {
	old := &model.Config{Sensors: []model.Sensor{
		sensor("smart-disk-1", model.Sensor{
			Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1",
			WarnC: 45, CritC: 55, Disabled: true, // hand-tuned away from the discovery default
		}),
	}}
	fresh := &model.Config{Sensors: []model.Sensor{
		// Same identity, independently generated ID, discovery's stock
		// default thresholds and enabled.
		sensor("smart-disk-9", model.Sensor{
			Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1",
			WarnC: 50, CritC: 60, Disabled: false,
		}),
	}}

	rep := Merge(old, fresh)
	if rep.SensorOverrides != 1 {
		t.Fatalf("expected SensorOverrides=1, got %d", rep.SensorOverrides)
	}
	got := fresh.Sensors[0]
	if got.WarnC != 45 || got.CritC != 55 || !got.Disabled {
		t.Errorf("expected old thresholds/disabled to be reapplied, got %+v", got)
	}
	if got.ID != "smart-disk-9" {
		t.Errorf("expected the freshly generated ID to be kept, got %q", got.ID)
	}
}

func TestMergeRecomputesRequiredAfterDisablingAGroup(t *testing.T) {
	// Regression: Run() computes Required from the group state BEFORE
	// Merge changes Enabled - if Merge didn't re-derive it afterward, a
	// sensor whose group just got disabled would keep Required=true and
	// could trip the safety fallback for a sensor no longer driving
	// anything.
	old := &model.Config{
		Sensors: []model.Sensor{
			sensor("smart-disk-1", model.Sensor{Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1"}),
		},
		Groups: []model.Group{
			{Name: "disks", SensorIDs: []string{"smart-disk-1"}, Aggregation: model.AggAvg, Enabled: false}, // hand-disabled
		},
	}
	fresh := &model.Config{
		Sensors: []model.Sensor{
			sensor("smart-disk-9", model.Sensor{
				Source: model.SourceSMART, Device: "/dev/bus/0", DeviceType: "megaraid,1",
				Required: true, // as Run()'s markRequired would set it, group still enabled at that point
			}),
		},
		Groups: []model.Group{
			{Name: "disks", SensorIDs: []string{"smart-disk-9"}, Aggregation: model.AggAvg, Enabled: true},
		},
	}

	Merge(old, fresh)

	if fresh.Sensors[0].Required {
		t.Errorf("expected Required to be cleared once its group was merged to disabled, got true")
	}
	if fresh.Groups[0].Enabled {
		t.Errorf("expected the disks group to end up disabled (from old), got enabled")
	}
}

func TestMergeIsEmptyWhenNothingToCarryOver(t *testing.T) {
	old := &model.Config{}
	fresh := &model.Config{}
	rep := Merge(old, fresh)
	// MQTT is always "preserved" (copied, even if zero-value), so the
	// report is never truly empty once a merge runs at all - only
	// IsEmpty() (used to decide whether to print anything) should reflect
	// that copying a zero-value struct over another isn't worth reporting.
	if !rep.IsEmpty() {
		t.Errorf("expected an empty report when there's nothing beyond a no-op MQTT copy, got %+v", rep)
	}
}
