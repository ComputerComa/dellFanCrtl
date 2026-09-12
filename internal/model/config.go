// Package model holds the on-disk configuration shape shared by discovery,
// the control loop, and the CLI. Keeping it dependency-free (stdlib + yaml
// tags only) avoids import cycles between those packages.
package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Duration wraps time.Duration so it (de)serializes as a human string like
// "5s" in YAML/JSON instead of an opaque integer of nanoseconds.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// SensorSource identifies where a sensor's live reading comes from.
type SensorSource string

const (
	SourceIPMI  SensorSource = "ipmi"
	SourceSMART SensorSource = "smart"
)

// SensorClass is the logical role a sensor plays, used both for display and
// to auto-assign sensors to curve groups during discovery.
type SensorClass string

const (
	ClassCPU         SensorClass = "cpu"
	ClassInlet       SensorClass = "inlet"
	ClassExhaust     SensorClass = "exhaust"
	ClassBoard       SensorClass = "board"
	ClassDisk        SensorClass = "disk"
	ClassMemory      SensorClass = "memory"
	ClassPSU         SensorClass = "psu"
	ClassFanRPM      SensorClass = "fan_rpm"
	ClassUtilization SensorClass = "utilization"
	ClassPower       SensorClass = "power"
	ClassOther       SensorClass = "other"
)

// Sensor describes one discovered, individually-readable sensor.
type Sensor struct {
	ID     string       `yaml:"id"`
	Name   string       `yaml:"name"`
	Class  SensorClass  `yaml:"class"`
	Source SensorSource `yaml:"source"`

	// IPMI-specific. Some boards report multiple sensors under the exact
	// same name (e.g. two sensors both literally named "Temp" for CPU1/
	// CPU2). Occurrence is the 1-based index of this sensor among all
	// `ipmitool sensor list` rows sharing IPMIName, in the order ipmitool
	// reports them (stable across invocations); 1 for any non-duplicated
	// name.
	IPMIName   string `yaml:"ipmi_name,omitempty"`
	Occurrence int    `yaml:"occurrence,omitempty"`

	// SMART-specific.
	Device     string `yaml:"device,omitempty"`      // e.g. /dev/bus/0
	DeviceType string `yaml:"device_type,omitempty"` // smartctl -d argument, e.g. megaraid,1

	// Required sensors that go stale trigger the safety fallback; disabled
	// sensors are skipped by discovery-derived defaults but kept so the
	// config stays a full record of what was found.
	Required bool `yaml:"required"`
	Disabled bool `yaml:"disabled,omitempty"`

	// Temperature-class thresholds, in Celsius. WarnC is informational
	// (logged); CritC triggers the safety fallback to iDRAC automatic
	// control. Zero means "not set" (use the group/global default).
	WarnC float64 `yaml:"warn_c,omitempty"`
	CritC float64 `yaml:"crit_c,omitempty"`

	// Fan-RPM-class only: minimum acceptable RPM. Below this, the sensor
	// is treated as a failed/failing fan and trips the safety fallback.
	MinRPM float64 `yaml:"min_rpm,omitempty"`
}

// CurvePoint is one (temperature, fan percent) anchor of a fan curve. Points
// are linearly interpolated between; below the first point the first point's
// percent applies, above the last the last point's percent applies.
type CurvePoint struct {
	TempC  float64 `yaml:"temp_c"`
	FanPct float64 `yaml:"fan_pct"`
}

// AggregationMode combines multiple sensors' readings into one group value.
type AggregationMode string

const (
	AggAvg AggregationMode = "avg"
	AggMax AggregationMode = "max"
	AggMin AggregationMode = "min"
)

// Group is a set of sensors that are logically combined (e.g. all disk
// temperatures averaged together) and driven through a single fan curve.
type Group struct {
	Name        string          `yaml:"name"`
	SensorIDs   []string        `yaml:"sensor_ids"`
	Aggregation AggregationMode `yaml:"aggregation"`
	Curve       []CurvePoint    `yaml:"curve"`
	// Enabled=false keeps a discovered grouping in the file for reference
	// without it influencing the fan speed decision.
	Enabled bool `yaml:"enabled"`
}

// DellRaw holds the raw ipmitool byte sequences used to talk to the Dell
// iDRAC OEM fan-control interface. These are the widely-documented
// PowerEdge OEM commands (netfn 0x30); they're exposed here, rather than
// hardcoded, because some generations/BMC firmware need different zone
// bytes for the "set speed" command.
type DellRaw struct {
	// e.g. ["0x30","0x30","0x01","0x00"]
	ManualModeCmd []string `yaml:"manual_mode_cmd"`
	// e.g. ["0x30","0x30","0x01","0x01"]
	AutoModeCmd []string `yaml:"auto_mode_cmd"`
	// Prefix before the zone byte and percent byte, e.g. ["0x30","0x30","0x02"].
	SetSpeedPrefix []string `yaml:"set_speed_prefix"`
	// Zone byte(s) appended after SetSpeedPrefix, before the percent byte.
	// Default ["0xff"] targets "all zones" (13G and newer). Older
	// generations without a 0xff broadcast may need one command per fan,
	// e.g. ["0x00","0x01","0x02","0x03","0x04","0x05"].
	Zones []string `yaml:"zones"`
}

// Smoothing controls how raw sensor readings turn into a commanded fan
// percentage: an EMA damps noise/spikes, and step limits + a ramp-down hold
// keep the fan from hunting.
type Smoothing struct {
	// EMAAlpha in (0,1]; higher reacts faster to new readings, lower
	// smooths harder. 1.0 disables smoothing entirely.
	EMAAlpha float64 `yaml:"ema_alpha"`
	// Maximum fan-percent change applied per tick, up and down.
	MaxStepUpPct   float64 `yaml:"max_step_up_pct"`
	MaxStepDownPct float64 `yaml:"max_step_down_pct"`
	// Minimum change (percent) worth sending an ipmitool command for.
	MinChangePct float64 `yaml:"min_change_pct"`
	MinFanPct    float64 `yaml:"min_fan_pct"`
	MaxFanPct    float64 `yaml:"max_fan_pct"`
	// Consecutive ticks the desired speed must stay below the current
	// commanded speed before a ramp-down step is allowed. Prevents fans
	// diving and re-spiking on brief dips.
	RampDownHoldTicks int `yaml:"ramp_down_hold_ticks"`
}

// Safety controls the fallback that hands control back to the iDRAC.
type Safety struct {
	// Absolute ceiling applied to any required sensor without its own
	// CritC set.
	GlobalCritC float64 `yaml:"global_crit_c"`
	// Degrees below CritC a sensor must fall before it counts as
	// "recovered".
	RecoverHysteresisC float64 `yaml:"recover_hysteresis_c"`
	// Consecutive good ticks required before resuming manual control.
	RecoverGoodTicks int `yaml:"recover_good_ticks"`
	// Consecutive total polling failures (e.g. ipmitool erroring
	// repeatedly) before falling back, on the assumption we can no longer
	// see well enough to control fans safely.
	MaxConsecutiveErrors int `yaml:"max_consecutive_errors"`
	// Whether to issue the "return to iDRAC automatic control" command on
	// normal shutdown (SIGINT/SIGTERM) as well as on error. Defaults to
	// true; turning it off is only for advanced setups that manage the
	// handoff themselves.
	RevertOnExit bool `yaml:"revert_on_exit"`
}

// Logging controls verbosity of the running daemon.
type Logging struct {
	Level string `yaml:"level"` // debug, info, warn, error
}

// Config is the full on-disk configuration file.
type Config struct {
	Version      int       `yaml:"version"`
	IpmitoolPath string    `yaml:"ipmitool_path"`
	SmartctlPath string    `yaml:"smartctl_path"`
	PollInterval Duration  `yaml:"poll_interval"`
	Dell         DellRaw   `yaml:"dell"`
	Smoothing    Smoothing `yaml:"smoothing"`
	Safety       Safety    `yaml:"safety"`
	Sensors      []Sensor  `yaml:"sensors"`
	Groups       []Group   `yaml:"groups"`
	Logging      Logging   `yaml:"logging"`
}

// SensorByID returns the sensor with the given ID, or nil.
func (c *Config) SensorByID(id string) *Sensor {
	for i := range c.Sensors {
		if c.Sensors[i].ID == id {
			return &c.Sensors[i]
		}
	}
	return nil
}

// Validate checks structural invariants that would otherwise surface as
// confusing runtime errors or, worse, silently-wrong fan behavior.
func (c *Config) Validate() error {
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll_interval must be > 0")
	}
	if len(c.Dell.ManualModeCmd) == 0 || len(c.Dell.AutoModeCmd) == 0 || len(c.Dell.SetSpeedPrefix) == 0 {
		return fmt.Errorf("dell raw command sequences must not be empty")
	}
	if len(c.Dell.Zones) == 0 {
		return fmt.Errorf("dell.zones must list at least one zone byte (use [\"0xff\"] for all-zone boards)")
	}
	if c.Smoothing.EMAAlpha <= 0 || c.Smoothing.EMAAlpha > 1 {
		return fmt.Errorf("smoothing.ema_alpha must be in (0,1]")
	}
	if c.Smoothing.MaxFanPct <= c.Smoothing.MinFanPct {
		return fmt.Errorf("smoothing.max_fan_pct must be greater than min_fan_pct")
	}
	if c.Smoothing.MaxStepUpPct <= 0 || c.Smoothing.MaxStepDownPct <= 0 {
		return fmt.Errorf("smoothing max_step_up_pct/max_step_down_pct must be > 0")
	}
	seen := map[string]bool{}
	for _, s := range c.Sensors {
		if s.ID == "" {
			return fmt.Errorf("sensor with empty id: %+v", s)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate sensor id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Source == SourceIPMI && s.IPMIName == "" {
			return fmt.Errorf("sensor %q: source ipmi requires ipmi_name", s.ID)
		}
		if s.Source == SourceSMART && (s.Device == "" || s.DeviceType == "") {
			return fmt.Errorf("sensor %q: source smart requires device and device_type", s.ID)
		}
	}
	groupNames := map[string]bool{}
	for _, g := range c.Groups {
		if g.Name == "" {
			return fmt.Errorf("group with empty name")
		}
		if groupNames[g.Name] {
			return fmt.Errorf("duplicate group name %q", g.Name)
		}
		groupNames[g.Name] = true
		if !g.Enabled {
			continue
		}
		if len(g.SensorIDs) == 0 {
			return fmt.Errorf("group %q: no sensor_ids", g.Name)
		}
		for _, id := range g.SensorIDs {
			if seen[id] == false {
				return fmt.Errorf("group %q references unknown sensor id %q", g.Name, id)
			}
		}
		switch g.Aggregation {
		case AggAvg, AggMax, AggMin:
		default:
			return fmt.Errorf("group %q: invalid aggregation %q", g.Name, g.Aggregation)
		}
		if len(g.Curve) < 1 {
			return fmt.Errorf("group %q: curve needs at least one point", g.Name)
		}
		pts := append([]CurvePoint(nil), g.Curve...)
		sort.Slice(pts, func(i, j int) bool { return pts[i].TempC < pts[j].TempC })
		for i := range pts {
			if pts[i] != g.Curve[i] {
				return fmt.Errorf("group %q: curve points must be sorted by ascending temp_c", g.Name)
			}
		}
	}
	return nil
}

// HasEnabledGroups reports whether at least one group actively drives the
// fan curve, which "run" requires to do anything useful.
func (c *Config) HasEnabledGroups() bool {
	for _, g := range c.Groups {
		if g.Enabled {
			return true
		}
	}
	return false
}
