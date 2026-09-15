// Package discover probes IPMI sensors and SMART-capable disks, classifies
// what it finds, and produces a ready-to-review Config with sensible
// default groupings and fan curves.
package discover

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dellfanctl/internal/classify"
	"dellfanctl/internal/ipmi"
	"dellfanctl/internal/model"
	"dellfanctl/internal/smart"
)

// Options controls what discovery probes.
type Options struct {
	IpmitoolPath string
	SmartctlPath string
	SkipIPMI     bool
	SkipSMART    bool
	Logf         func(format string, args ...interface{})
}

func (o Options) log(format string, args ...interface{}) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(s)
	s = slugNonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// uniqueID returns a slug of name guaranteed not to collide with anything
// already in used, adding a numeric suffix if needed, and records it.
func uniqueID(used map[string]bool, name string) string {
	base := slug(name)
	if base == "" {
		base = "sensor"
	}
	id := base
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	used[id] = true
	return id
}

// Run performs discovery and returns a Config with Sensors and Groups
// populated. It only reads hardware state; it never issues fan-control
// commands.
func Run(ctx context.Context, opts Options) (*model.Config, error) {
	cfg := model.Default()
	if opts.IpmitoolPath != "" {
		cfg.IpmitoolPath = opts.IpmitoolPath
	}
	if opts.SmartctlPath != "" {
		cfg.SmartctlPath = opts.SmartctlPath
	}
	usedIDs := map[string]bool{}
	cpuOccurrence := 0

	if !opts.SkipIPMI {
		sensors, err := ipmi.ListSensors(ctx, cfg.IpmitoolPath)
		if err != nil {
			return nil, fmt.Errorf("listing ipmi sensors: %w", err)
		}
		entities, err := ipmi.ListEntities(ctx, cfg.IpmitoolPath)
		if err != nil {
			opts.log("warning: could not read sensor entities (%v); generic sensor names may be ambiguous", err)
		} else {
			ipmi.MergeEntities(sensors, entities)
		}

		nameOccurrence := map[string]int{}
		for _, s := range sensors {
			// Counted over every row regardless of what's kept below, so
			// this occurrence index matches what the runtime control loop
			// computes the same way over its own (unfiltered) sensor list
			// call each tick — needed because some boards (including the
			// one this was developed against) report multiple sensors
			// under the exact same name, e.g. two sensors both literally
			// named "Temp" for CPU1/CPU2.
			nameOccurrence[s.Name]++
			occurrence := nameOccurrence[s.Name]

			if !s.HasReading {
				continue
			}
			unit := strings.ToLower(s.Unit)
			if !strings.Contains(unit, "degrees") && !strings.Contains(unit, "rpm") {
				// Skip non-temp/fan analog sensors (usage %, voltage,
				// current, power draw) for now: they add noise to a
				// fan-control config without being obviously actionable.
				// They're easy to add back by hand.
				continue
			}
			class := classify.IPMISensor(s.Name, s.Unit, s.Entity)
			if class == model.ClassCPU {
				cpuOccurrence++
			}
			display := classify.DisplayName(s.Name, class, cpuOccurrence)
			id := uniqueID(usedIDs, "ipmi-"+display)

			sensor := model.Sensor{
				ID:         id,
				Name:       display,
				Class:      class,
				Source:     model.SourceIPMI,
				IPMIName:   s.Name,
				Occurrence: occurrence,
			}
			switch class {
			case model.ClassFanRPM:
				min := s.Reading * 0.3
				if min < 300 {
					min = 300
				}
				sensor.MinRPM = round1(min)
			case model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard:
				if s.HasUNC {
					sensor.WarnC = s.UNC
				}
				if s.HasUCR {
					sensor.CritC = s.UCR
				}
			}
			cfg.Sensors = append(cfg.Sensors, sensor)
			opts.log("ipmi sensor: %-20s class=%-6s reading=%.1f%s", display, class, s.Reading, unitSuffix(s.Unit))
		}
	}

	if !opts.SkipSMART {
		devices, err := smart.Scan(ctx, cfg.SmartctlPath)
		if err != nil {
			opts.log("warning: smartctl scan failed (%v); skipping disk discovery", err)
		}
		diskNum := 0
		for _, d := range devices {
			info, err := smart.ReadInfo(ctx, cfg.SmartctlPath, d.Name, d.Type)
			if err != nil {
				opts.log("warning: could not read %s (-d %s): %v", d.Name, d.Type, err)
				continue
			}
			if !info.HasTemp {
				opts.log("skipping %s (-d %s): no temperature reported", d.Name, d.Type)
				continue
			}
			diskNum++
			label := info.Model
			if label == "" {
				label = d.Type
			}
			display := fmt.Sprintf("Disk %d Temp (%s)", diskNum, label)
			id := uniqueID(usedIDs, fmt.Sprintf("smart-disk-%d", diskNum))
			cfg.Sensors = append(cfg.Sensors, model.Sensor{
				ID:         id,
				Name:       display,
				Class:      model.ClassDisk,
				Source:     model.SourceSMART,
				Device:     d.Name,
				DeviceType: d.Type,
				WarnC:      50,
				CritC:      60,
			})
			opts.log("smart disk: %-30s serial=%-20s reading=%.1fC", display, info.Serial, info.TempC)
		}
	}

	cfg.Groups = buildGroups(cfg.Sensors)
	markRequired(&cfg)

	if len(cfg.Sensors) == 0 {
		return nil, fmt.Errorf("no sensors discovered (ipmitool/smartctl unavailable or produced no usable data)")
	}
	return &cfg, nil
}

// DiffResult summarizes drift between a previously generated config's
// sensors and what a fresh probe (Run) finds right now. It's purely a
// report: computing it never touches hardware beyond what Run itself does
// (read-only), and never modifies the old config.
type DiffResult struct {
	// Missing are sensors present in the old config that the fresh probe
	// couldn't confirm - the case that matters most, since a Required one
	// going stale is what trips the safety fallback (see internal/control).
	Missing []model.Sensor
	// New are sensors the fresh probe found with no corresponding entry in
	// the old config - added capacity, not yet reviewed or monitored.
	New []model.Sensor
}

// HasChanges reports whether Diff found any drift at all.
func (r DiffResult) HasChanges() bool {
	return len(r.Missing) > 0 || len(r.New) > 0
}

// identityKey is what a sensor is matched on across two probes: for SMART,
// its device+device_type (the physical controller slot address, which is
// what stays stable across a same-slot drive swap - see the SMART-specific
// fields' doc comment in internal/model); for IPMI, its name+occurrence.
// Deliberately not Sensor.ID, which is independently slugged per run and
// isn't guaranteed to match across two separate discovery passes.
func identityKey(s model.Sensor) string {
	switch s.Source {
	case model.SourceSMART:
		return "smart:" + s.Device + ":" + s.DeviceType
	case model.SourceIPMI:
		return fmt.Sprintf("ipmi:%s:%d", s.IPMIName, s.Occurrence)
	default:
		return "other:" + s.ID
	}
}

// Diff compares an existing config's sensors against a freshly discovered
// probe (typically the result of calling Run again with the same Options).
func Diff(old, fresh *model.Config) DiffResult {
	oldByKey := make(map[string]model.Sensor, len(old.Sensors))
	for _, s := range old.Sensors {
		oldByKey[identityKey(s)] = s
	}
	freshByKey := make(map[string]model.Sensor, len(fresh.Sensors))
	for _, s := range fresh.Sensors {
		freshByKey[identityKey(s)] = s
	}

	var res DiffResult
	for k, s := range oldByKey {
		if _, ok := freshByKey[k]; !ok {
			res.Missing = append(res.Missing, s)
		}
	}
	for k, s := range freshByKey {
		if _, ok := oldByKey[k]; !ok {
			res.New = append(res.New, s)
		}
	}
	sort.Slice(res.Missing, func(i, j int) bool { return res.Missing[i].ID < res.Missing[j].ID })
	sort.Slice(res.New, func(i, j int) bool { return res.New[i].ID < res.New[j].ID })
	return res
}

// MergeReport summarizes what Merge carried forward from an existing
// config into a freshly discovered one.
type MergeReport struct {
	MQTTPreserved bool
	// CurvesPreserved are group names whose curve/enabled/aggregation came
	// from the old config instead of the freshly generated default.
	CurvesPreserved []string
	// GroupsCarriedOver are groups only present in the old config (e.g.
	// hand-added) that Run's classifier wouldn't have generated at all,
	// copied over unchanged.
	GroupsCarriedOver []string
	// SensorOverrides counts sensors, matched by identity (see
	// identityKey) rather than ID, whose warn_c/crit_c/disabled were
	// reapplied from the old config.
	SensorOverrides int
}

func (r MergeReport) IsEmpty() bool {
	return !r.MQTTPreserved && len(r.CurvesPreserved) == 0 && len(r.GroupsCarriedOver) == 0 && r.SensorOverrides == 0
}

// Merge overlays the settings a human is expected to hand-tune (see
// config.example.yaml's "review before running" guidance) from old onto
// fresh, in place, so that re-running discovery to pick up a hardware
// change (see Diff) doesn't also throw away curves, MQTT settings, or
// per-sensor thresholds nobody asked to reset:
//
//   - MQTT is copied wholesale: discovery never touches it in the first
//     place, so there's nothing to merge, only to not lose.
//   - Each known group (matched by Name - "cpu", "disks", etc.) keeps its
//     old Curve/Enabled/Aggregation; SensorIDs stays whatever Run just
//     computed, since that must reflect which sensors actually exist now.
//   - Sensors are matched by identity, not ID (Sensor IDs are
//     independently slugged per discovery pass and aren't guaranteed to
//     match up - see identityKey); a matched sensor keeps its old
//     WarnC/CritC/Disabled. This is safe even for values that merely
//     happen to equal a past default: IPMI thresholds come from the BMC's
//     own reporting (unlikely to regress), and SMART disk thresholds are
//     a flat code default regardless of drive model, so "old value" and
//     "hand-tuned value" are, in practice, close enough to the same thing
//     to not bother distinguishing.
//   - A group present only in old (Run's classifier didn't reconstruct
//     it - most likely a hand-added custom group) is copied over as-is;
//     its SensorIDs are NOT revalidated, since they may reference IDs
//     that no longer exist post-regeneration (report this to the caller
//     so it can tell the operator to check).
//
// Callers should still tell the operator to dry-run and sanity-check the
// preserved curves afterward: SensorIDs changing under an unchanged curve
// means the same curve is now fed different real-world values.
func Merge(old, fresh *model.Config) MergeReport {
	var rep MergeReport

	// Only worth reporting (and IsEmpty caring about) if old.MQTT actually
	// held something; copying a zero-value struct over another zero-value
	// isn't "preserving" anything a human would notice.
	if old.MQTT != (model.MQTT{}) {
		rep.MQTTPreserved = true
	}
	fresh.MQTT = old.MQTT

	oldGroups := make(map[string]model.Group, len(old.Groups))
	for _, g := range old.Groups {
		oldGroups[g.Name] = g
	}
	freshGroupNames := make(map[string]bool, len(fresh.Groups))
	for i := range fresh.Groups {
		freshGroupNames[fresh.Groups[i].Name] = true
		og, ok := oldGroups[fresh.Groups[i].Name]
		if !ok {
			continue
		}
		fresh.Groups[i].Curve = og.Curve
		fresh.Groups[i].Enabled = og.Enabled
		fresh.Groups[i].Aggregation = og.Aggregation
		rep.CurvesPreserved = append(rep.CurvesPreserved, fresh.Groups[i].Name)
	}
	for _, g := range old.Groups {
		if !freshGroupNames[g.Name] {
			fresh.Groups = append(fresh.Groups, g)
			rep.GroupsCarriedOver = append(rep.GroupsCarriedOver, g.Name)
		}
	}

	oldByKey := make(map[string]model.Sensor, len(old.Sensors))
	for _, s := range old.Sensors {
		oldByKey[identityKey(s)] = s
	}
	for i := range fresh.Sensors {
		os, ok := oldByKey[identityKey(fresh.Sensors[i])]
		if !ok {
			continue
		}
		fresh.Sensors[i].WarnC = os.WarnC
		fresh.Sensors[i].CritC = os.CritC
		fresh.Sensors[i].Disabled = os.Disabled
		rep.SensorOverrides++
	}

	// Re-derive Required from the group state as it stands now: merging
	// may just have disabled (or enabled) a group, and Required has to
	// track that, not the pre-merge snapshot Run() computed it from.
	markRequired(fresh)

	sort.Strings(rep.CurvesPreserved)
	sort.Strings(rep.GroupsCarriedOver)
	return rep
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return " " + unit
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

func idsOfClass(sensors []model.Sensor, class model.SensorClass) []string {
	var ids []string
	for _, s := range sensors {
		if s.Class == class {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// buildGroups assembles the default curve groups from classified sensors.
// "cpu", "disks", and "system" are enabled by default and drive the fan
// curve; "inlet" and "exhaust" are recorded but disabled, since ambient
// intake/exhaust temperature alone is usually a poor primary signal (it
// reacts to room conditions, not component load) — left available for users
// who want to factor it in.
func buildGroups(sensors []model.Sensor) []model.Group {
	var groups []model.Group

	add := func(name string, class model.SensorClass, agg model.AggregationMode, enabled bool, curve []model.CurvePoint) {
		ids := idsOfClass(sensors, class)
		if len(ids) == 0 {
			return
		}
		groups = append(groups, model.Group{
			Name:        name,
			SensorIDs:   ids,
			Aggregation: agg,
			Enabled:     enabled,
			Curve:       curve,
		})
	}

	add("cpu", model.ClassCPU, model.AggMax, true, []model.CurvePoint{
		{TempC: 30, FanPct: 20},
		{TempC: 45, FanPct: 30},
		{TempC: 55, FanPct: 45},
		{TempC: 65, FanPct: 65},
		{TempC: 75, FanPct: 90},
		{TempC: 85, FanPct: 100},
	})
	add("disks", model.ClassDisk, model.AggAvg, true, []model.CurvePoint{
		{TempC: 25, FanPct: 20},
		{TempC: 35, FanPct: 30},
		{TempC: 40, FanPct: 45},
		{TempC: 45, FanPct: 65},
		{TempC: 50, FanPct: 85},
		{TempC: 55, FanPct: 100},
	})
	add("system", model.ClassBoard, model.AggMax, true, []model.CurvePoint{
		{TempC: 30, FanPct: 20},
		{TempC: 45, FanPct: 35},
		{TempC: 55, FanPct: 55},
		{TempC: 65, FanPct: 80},
		{TempC: 75, FanPct: 100},
	})
	add("inlet", model.ClassInlet, model.AggMax, false, []model.CurvePoint{
		{TempC: 20, FanPct: 15},
		{TempC: 30, FanPct: 30},
		{TempC: 35, FanPct: 50},
		{TempC: 40, FanPct: 80},
	})
	add("exhaust", model.ClassExhaust, model.AggMax, false, []model.CurvePoint{
		{TempC: 25, FanPct: 20},
		{TempC: 35, FanPct: 35},
		{TempC: 45, FanPct: 55},
		{TempC: 55, FanPct: 80},
		{TempC: 65, FanPct: 100},
	})

	return groups
}

// markRequired sets every sensor's Required flag to reflect cfg's current
// groups: true for every sensor referenced by an enabled group, plus all
// fan-RPM sensors; false for everything else. The control loop trips its
// safety fallback if a required sensor goes stale, since it can no longer
// be sure it's making a safe fan-speed decision - so this is a full,
// idempotent recompute (not just an "add" pass) rather than only ever
// setting Required=true, since Merge calls this again after changing a
// group's Enabled flag, and a sensor whose group *just got disabled* must
// have Required cleared, not left stale from before that change.
func markRequired(cfg *model.Config) {
	required := map[string]bool{}
	for _, g := range cfg.Groups {
		if !g.Enabled {
			continue
		}
		for _, id := range g.SensorIDs {
			required[id] = true
		}
	}
	for i := range cfg.Sensors {
		if cfg.Sensors[i].Class == model.ClassFanRPM {
			required[cfg.Sensors[i].ID] = true
		}
	}
	for i := range cfg.Sensors {
		cfg.Sensors[i].Required = required[cfg.Sensors[i].ID]
	}
}
