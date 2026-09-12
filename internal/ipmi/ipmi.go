// Package ipmi wraps ipmitool invocations: reading the sensor table and
// issuing the Dell iDRAC OEM raw commands that switch fan control between
// manual and automatic and set a manual fan speed.
package ipmi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dellfanctl/internal/execx"
	"dellfanctl/internal/model"
)

// Sensor is one row of `ipmitool sensor list`.
type Sensor struct {
	Name       string
	Reading    float64
	HasReading bool // false for "na" / discrete sensors
	Unit       string
	Status     string
	// Thresholds; Has[i] false means that column was "na".
	LNR, LCR, LNC, UNC, UCR, UNR                   float64
	HasLNR, HasLCR, HasLNC, HasUNC, HasUCR, HasUNR bool
	// Entity, filled in by MergeEntities from `sdr list full` (e.g. "3.1").
	Entity string
}

// ListSensors runs `ipmitool sensor list` and parses every row. Rows for
// discrete sensors (no numeric reading) are still returned with
// HasReading=false so callers can log/ignore them without a second pass.
func ListSensors(ctx context.Context, bin string) ([]Sensor, error) {
	out, err := execx.Run(ctx, 0, bin, "sensor", "list")
	if err != nil {
		return nil, err
	}
	var sensors []Sensor
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 4 {
			continue
		}
		s := Sensor{Name: fields[0], Status: fields[3]}
		if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
			s.Reading = v
			s.HasReading = true
			s.Unit = fields[2]
		}
		if len(fields) >= 10 {
			assign := func(s string) (float64, bool) {
				v, err := strconv.ParseFloat(s, 64)
				return v, err == nil
			}
			s.LNR, s.HasLNR = assign(fields[4])
			s.LCR, s.HasLCR = assign(fields[5])
			s.LNC, s.HasLNC = assign(fields[6])
			s.UNC, s.HasUNC = assign(fields[7])
			s.UCR, s.HasUCR = assign(fields[8])
			s.UNR, s.HasUNR = assign(fields[9])
		}
		sensors = append(sensors, s)
	}
	return sensors, nil
}

// ListEntities runs `ipmitool sdr elist full` and returns, per line in
// order, the sensor name and its entity id.instance (e.g. "3.1"). This
// command ("extended list") only lists "full" analog sensor records, which
// is an ordered subsequence of `sensor list`'s output (same underlying SDR
// walk, more record types). MergeEntities exploits that to attach entities
// by name in order rather than assuming a 1:1 line correspondence.
func ListEntities(ctx context.Context, bin string) ([]struct{ Name, Entity string }, error) {
	out, err := execx.Run(ctx, 0, bin, "sdr", "elist", "full")
	if err != nil {
		return nil, err
	}
	var rows []struct{ Name, Entity string }
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			continue
		}
		rows = append(rows, struct{ Name, Entity string }{
			Name:   strings.TrimSpace(fields[0]),
			Entity: strings.TrimSpace(fields[3]),
		})
	}
	return rows, nil
}

// MergeEntities attaches an Entity to each sensor by walking sensors and
// entities in lockstep: entities is an ordered subsequence of sensors (same
// names, same relative order, some sensors missing). A two-pointer scan
// matches each entity row to the next sensor with the same name.
func MergeEntities(sensors []Sensor, entities []struct{ Name, Entity string }) {
	ei := 0
	for si := range sensors {
		if ei >= len(entities) {
			return
		}
		if sensors[si].Name == entities[ei].Name {
			sensors[si].Entity = entities[ei].Entity
			ei++
		}
	}
}

// EntityProcessorPrefix is the IPMI Entity ID for "Processor" (per the IPMI
// Platform Management FRU/Entity ID table). Sensors whose entity starts
// with this are physically attached to a CPU socket, which is used to
// disambiguate generically-named "Temp" sensors as per-CPU temperatures.
const EntityProcessorPrefix = "3."

func buildRawArgs(bytesHex []string) []string {
	args := make([]string, 0, len(bytesHex)+1)
	args = append(args, "raw")
	args = append(args, bytesHex...)
	return args
}

// SetManualMode switches the BMC's fan control to manual, i.e. iDRAC stops
// adjusting fan speed on its own and honors SetFanPercent instead.
func SetManualMode(ctx context.Context, bin string, dell model.DellRaw) error {
	_, err := execx.Run(ctx, 0, bin, buildRawArgs(dell.ManualModeCmd)...)
	if err != nil {
		return fmt.Errorf("entering manual fan control: %w", err)
	}
	return nil
}

// SetAutoMode hands fan control back to the iDRAC's own dynamic algorithm.
// This is the safety fallback command: it is always safe to call, even if
// the BMC is already in automatic mode.
func SetAutoMode(ctx context.Context, bin string, dell model.DellRaw) error {
	_, err := execx.Run(ctx, 0, bin, buildRawArgs(dell.AutoModeCmd)...)
	if err != nil {
		return fmt.Errorf("returning fan control to iDRAC: %w", err)
	}
	return nil
}

// SetFanPercent commands all configured zones to pct percent (0-100). Must
// only be called while in manual mode (see SetManualMode).
func SetFanPercent(ctx context.Context, bin string, dell model.DellRaw, pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("fan percent %d out of range 0-100", pct)
	}
	pctHex := fmt.Sprintf("0x%02x", pct)
	for _, zone := range dell.Zones {
		args := buildRawArgs(dell.SetSpeedPrefix)
		args = append(args, zone, pctHex)
		if _, err := execx.Run(ctx, 0, bin, args...); err != nil {
			return fmt.Errorf("setting fan speed zone %s to %d%%: %w", zone, pct, err)
		}
	}
	return nil
}
