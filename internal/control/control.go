// Package control implements the auto-mode loop: poll sensors, smooth
// readings, compute a fan curve target, ramp toward it gradually, and fail
// safe back to the iDRAC's own automatic control if anything looks wrong.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"dellfanctl/internal/curve"
	"dellfanctl/internal/ipmi"
	"dellfanctl/internal/model"
	"dellfanctl/internal/smart"
)

// sensorState tracks the smoothing/staleness state for one configured
// sensor across ticks.
type sensorState struct {
	ema        float64
	emaInit    bool
	lastRaw    float64
	haveRaw    bool
	staleTicks int
}

// Controller runs the poll/compute/apply loop described in package docs.
type Controller struct {
	cfg    *model.Config
	log    *slog.Logger
	dryRun bool

	states map[string]*sensorState

	fallbackActive bool
	goodTicks      int
	commandedPct   float64 // -1 == unknown/unset
	belowHold      int
	consecutiveErr int
}

// New builds a Controller for cfg. log may be nil to use slog's default
// logger. dryRun, when true, logs every ipmitool action it would take
// without executing it.
func New(cfg *model.Config, log *slog.Logger, dryRun bool) *Controller {
	if log == nil {
		log = slog.Default()
	}
	states := make(map[string]*sensorState, len(cfg.Sensors))
	for _, s := range cfg.Sensors {
		states[s.ID] = &sensorState{}
	}
	return &Controller{
		cfg:          cfg,
		log:          log,
		dryRun:       dryRun,
		states:       states,
		commandedPct: -1,
	}
}

// Run drives the control loop until ctx is canceled (SIGINT/SIGTERM should
// cancel it upstream) or an unrecoverable error occurs. If once is true, it
// performs exactly one tick and returns instead of looping. On exit it
// reverts fan control to the iDRAC automatic algorithm unless the config
// disables that (Safety.RevertOnExit=false).
func (c *Controller) Run(ctx context.Context, once bool) error {
	if !c.cfg.HasEnabledGroups() {
		return fmt.Errorf("config has no enabled groups; nothing would drive the fan curve (edit the config or re-run discover)")
	}

	if err := c.enterManual(ctx); err != nil {
		return err
	}
	defer c.revertOnExit()

	if once {
		c.tick(ctx)
		return nil
	}

	interval := time.Duration(c.cfg.PollInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.log.Info("dellfanctl running", "poll_interval", interval, "dry_run", c.dryRun)
	for {
		select {
		case <-ctx.Done():
			c.log.Info("shutting down")
			return nil
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

func (c *Controller) enterManual(ctx context.Context) error {
	if c.dryRun {
		c.log.Info("[dry-run] would enter manual fan control")
		return nil
	}
	c.log.Info("entering manual fan control")
	return ipmi.SetManualMode(ctx, c.cfg.IpmitoolPath, c.cfg.Dell)
}

// revertOnExit hands control back to the iDRAC on the way out. It uses a
// fresh, short-lived context since the caller's ctx is likely already
// canceled by the time defer runs.
func (c *Controller) revertOnExit() {
	if !c.cfg.Safety.RevertOnExit {
		return
	}
	if c.dryRun {
		c.log.Info("[dry-run] would return fan control to iDRAC on exit")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.log.Info("returning fan control to iDRAC before exit")
	if err := ipmi.SetAutoMode(ctx, c.cfg.IpmitoolPath, c.cfg.Dell); err != nil {
		c.log.Error("failed to return fan control to iDRAC on exit; check hardware manually", "error", err)
	}
}

// poll reads every enabled sensor (IPMI in one batched call, SMART disks
// concurrently since each is a separate subprocess) and updates c.states.
func (c *Controller) poll(ctx context.Context) {
	ipmiSensors, ipmiErr := ipmi.ListSensors(ctx, c.cfg.IpmitoolPath)
	// Keyed by name to a slice in ipmitool's reported order, since some
	// boards report multiple sensors under the exact same name (e.g. two
	// sensors both named "Temp" for CPU1/CPU2); Sensor.Occurrence (set at
	// discovery time, see discover.go) picks the right one out of the
	// slice. This must count every row exactly as discovery did, so it is
	// built before any filtering.
	var ipmiByName map[string][]ipmi.Sensor
	if ipmiErr == nil {
		ipmiByName = make(map[string][]ipmi.Sensor, len(ipmiSensors))
		for _, s := range ipmiSensors {
			ipmiByName[s.Name] = append(ipmiByName[s.Name], s)
		}
	} else {
		c.log.Warn("ipmi sensor poll failed", "error", ipmiErr)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex // guards c.states writes from SMART goroutines

	for _, sc := range c.cfg.Sensors {
		if sc.Disabled {
			continue
		}
		st := c.states[sc.ID]
		switch sc.Source {
		case model.SourceIPMI:
			if ipmiByName == nil {
				c.markStale(st)
				continue
			}
			occ := sc.Occurrence
			if occ < 1 {
				occ = 1
			}
			rows := ipmiByName[sc.IPMIName]
			if occ > len(rows) || !rows[occ-1].HasReading {
				c.markStale(st)
				continue
			}
			c.recordReading(st, rows[occ-1].Reading)
		case model.SourceSMART:
			sc := sc
			wg.Add(1)
			go func() {
				defer wg.Done()
				temp, err := smart.ReadTemp(ctx, c.cfg.SmartctlPath, sc.Device, sc.DeviceType)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					c.log.Warn("smart poll failed", "sensor", sc.ID, "error", err)
					c.markStale(st)
					return
				}
				c.recordReading(st, temp)
			}()
		}
	}
	wg.Wait()

	if ipmiErr != nil {
		c.consecutiveErr++
	} else {
		c.consecutiveErr = 0
	}
}

func (c *Controller) recordReading(st *sensorState, raw float64) {
	st.lastRaw = raw
	st.haveRaw = true
	st.staleTicks = 0
	if !st.emaInit {
		st.ema = raw
		st.emaInit = true
	} else {
		a := c.cfg.Smoothing.EMAAlpha
		st.ema = a*raw + (1-a)*st.ema
	}
}

func (c *Controller) markStale(st *sensorState) {
	st.staleTicks++
}

// safetyTrip returns a non-empty reason if any required sensor currently
// warrants handing control back to the iDRAC.
func (c *Controller) safetyTrip() string {
	if c.consecutiveErr >= c.cfg.Safety.MaxConsecutiveErrors {
		return fmt.Sprintf("ipmi sensor polling failed %d times in a row", c.consecutiveErr)
	}
	for _, sc := range c.cfg.Sensors {
		if sc.Disabled {
			continue
		}
		st := c.states[sc.ID]
		if sc.Required && st.staleTicks > c.cfg.Safety.MaxConsecutiveErrors {
			return fmt.Sprintf("required sensor %q unreadable for %d ticks", sc.Name, st.staleTicks)
		}
		if !st.haveRaw {
			continue
		}
		switch sc.Class {
		case model.ClassFanRPM:
			if sc.MinRPM > 0 && st.lastRaw < sc.MinRPM {
				return fmt.Sprintf("fan sensor %q reads %.0f RPM, below minimum %.0f (possible fan failure)", sc.Name, st.lastRaw, sc.MinRPM)
			}
		case model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard, model.ClassDisk:
			crit := sc.CritC
			if crit <= 0 {
				crit = c.cfg.Safety.GlobalCritC
			}
			if crit > 0 && st.lastRaw >= crit {
				return fmt.Sprintf("sensor %q at %.1fC reached critical threshold %.1fC", sc.Name, st.lastRaw, crit)
			}
		}
	}
	return ""
}

// safetyClear reports whether every condition that could have tripped
// safetyTrip has cleared with hysteresis margin, i.e. it's safe to resume
// manual control.
func (c *Controller) safetyClear() bool {
	if c.consecutiveErr > 0 {
		return false
	}
	for _, sc := range c.cfg.Sensors {
		if sc.Disabled {
			continue
		}
		st := c.states[sc.ID]
		if sc.Required && (st.staleTicks > 0 || !st.haveRaw) {
			return false
		}
		switch sc.Class {
		case model.ClassFanRPM:
			if sc.MinRPM > 0 && st.haveRaw && st.lastRaw < sc.MinRPM {
				return false
			}
		case model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard, model.ClassDisk:
			crit := sc.CritC
			if crit <= 0 {
				crit = c.cfg.Safety.GlobalCritC
			}
			if crit > 0 && st.haveRaw && st.lastRaw >= crit-c.cfg.Safety.RecoverHysteresisC {
				return false
			}
		}
	}
	return true
}

func (c *Controller) aggregate(g model.Group) (float64, bool) {
	var vals []float64
	for _, id := range g.SensorIDs {
		st, ok := c.states[id]
		if !ok || !st.emaInit {
			continue
		}
		vals = append(vals, st.ema)
	}
	if len(vals) == 0 {
		return 0, false
	}
	switch g.Aggregation {
	case model.AggMax:
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m, true
	case model.AggMin:
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m, true
	default: // avg
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		return sum / float64(len(vals)), true
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// tick runs one full poll/decide/apply cycle.
func (c *Controller) tick(ctx context.Context) {
	c.poll(ctx)

	if reason := c.safetyTrip(); reason != "" {
		if !c.fallbackActive {
			c.log.Error("SAFETY FALLBACK: returning fan control to iDRAC", "reason", reason)
			if !c.dryRun {
				if err := ipmi.SetAutoMode(ctx, c.cfg.IpmitoolPath, c.cfg.Dell); err != nil {
					c.log.Error("failed to switch to automatic fan control", "error", err)
				}
			} else {
				c.log.Info("[dry-run] would return fan control to iDRAC")
			}
			c.fallbackActive = true
			c.commandedPct = -1
			c.goodTicks = 0
		}
		return
	}

	if c.fallbackActive {
		if c.safetyClear() {
			c.goodTicks++
			c.log.Info("conditions recovering", "good_ticks", c.goodTicks, "need", c.cfg.Safety.RecoverGoodTicks)
			if c.goodTicks >= c.cfg.Safety.RecoverGoodTicks {
				c.log.Info("resuming manual fan control")
				if !c.dryRun {
					if err := ipmi.SetManualMode(ctx, c.cfg.IpmitoolPath, c.cfg.Dell); err != nil {
						c.log.Error("failed to resume manual fan control", "error", err)
						return
					}
				} else {
					c.log.Info("[dry-run] would resume manual fan control")
				}
				c.fallbackActive = false
				c.goodTicks = 0
				c.belowHold = 0
			}
		} else {
			c.goodTicks = 0
		}
		return
	}

	desired := 0.0
	haveAny := false
	summary := make(map[string]float64, len(c.cfg.Groups))
	for _, g := range c.cfg.Groups {
		if !g.Enabled {
			continue
		}
		agg, ok := c.aggregate(g)
		if !ok {
			c.log.Warn("group has no readable sensors this tick; skipping its contribution", "group", g.Name)
			continue
		}
		pct := curve.Interpolate(g.Curve, agg)
		summary[g.Name] = agg
		if pct > desired {
			desired = pct
		}
		haveAny = true
	}
	if !haveAny {
		c.log.Warn("no enabled group produced a reading this tick; holding last commanded speed")
		return
	}
	desired = clamp(desired, c.cfg.Smoothing.MinFanPct, c.cfg.Smoothing.MaxFanPct)

	newPct := c.rampToward(desired)
	c.log.Info("tick", "groups", summary, "desired_pct", round1(desired), "commanded_pct", round1(newPct))

	if c.commandedPct < 0 || math.Abs(newPct-c.commandedPct) >= c.cfg.Smoothing.MinChangePct {
		if c.dryRun {
			c.log.Info("[dry-run] would set fan speed", "pct", round1(newPct))
		} else if err := ipmi.SetFanPercent(ctx, c.cfg.IpmitoolPath, c.cfg.Dell, int(math.Round(newPct))); err != nil {
			c.log.Error("failed to set fan speed", "error", err)
		}
	}
	c.commandedPct = newPct
}

// rampToward moves c.commandedPct toward desired by at most one tick's step
// limit, holding steady on decreases until RampDownHoldTicks consecutive
// ticks have asked for a lower speed (anti-flapping).
func (c *Controller) rampToward(desired float64) float64 {
	sm := c.cfg.Smoothing
	if c.commandedPct < 0 {
		c.belowHold = 0
		return clamp(desired, sm.MinFanPct, sm.MaxFanPct)
	}
	delta := desired - c.commandedPct
	var next float64
	switch {
	case delta > 0:
		c.belowHold = 0
		next = c.commandedPct + math.Min(delta, sm.MaxStepUpPct)
	case delta < 0:
		c.belowHold++
		if c.belowHold >= sm.RampDownHoldTicks {
			next = c.commandedPct + math.Max(delta, -sm.MaxStepDownPct)
		} else {
			next = c.commandedPct
		}
	default:
		c.belowHold = 0
		next = c.commandedPct
	}
	return clamp(next, sm.MinFanPct, sm.MaxFanPct)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
