// Package control implements the auto-mode loop: poll sensors, smooth
// readings, compute a fan curve target, ramp toward it gradually, and fail
// safe back to the iDRAC's own automatic control if anything looks wrong.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"dellfanctl/internal/curve"
	"dellfanctl/internal/ipmi"
	"dellfanctl/internal/model"
	"dellfanctl/internal/mqttpub"
	"dellfanctl/internal/smart"
)

// hookTimeout bounds an on_fallback_cmd/on_recover_cmd invocation, matching
// execx's default for external commands elsewhere in this codebase.
const hookTimeout = 10 * time.Second

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
	cfg     *model.Config
	log     *slog.Logger
	dryRun  bool
	mqttPub *mqttpub.Publisher
	mqttWG  sync.WaitGroup

	states map[string]*sensorState

	fallbackActive bool
	fallbackReason string
	goodTicks      int
	commandedPct   float64 // -1 == unknown/unset
	belowHold      int
	consecutiveErr int

	hookWG sync.WaitGroup
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

// SetMQTT attaches an MQTT publisher that Run will announce discovery to
// and Close on exit, and that every tick will publish a snapshot to. Call
// before Run; passing nil (the default) simply disables MQTT publishing.
func (c *Controller) SetMQTT(pub *mqttpub.Publisher) {
	c.mqttPub = pub
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

	if c.mqttPub != nil {
		c.mqttPub.PublishDiscovery(c.cfg)
		defer func() {
			// Let any in-flight publishAsync goroutine finish (each is
			// itself bounded, see mqttpub.PublishSnapshot) before
			// disconnecting, so shutdown can't cut off a publish that's
			// still queued.
			c.mqttWG.Wait()
			c.mqttPub.Close()
		}()
	}
	// Similarly, don't let the process exit out from under a still-running
	// on_fallback_cmd/on_recover_cmd (each is itself bounded by
	// hookTimeout, so this can't hang shutdown).
	defer c.hookWG.Wait()

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

// isTempClass reports whether a sensor class reports a Celsius reading,
// used both by the safety checks above and to compute the MQTT "overall
// average temperature" summary.
func isTempClass(c model.SensorClass) bool {
	switch c {
	case model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard, model.ClassDisk, model.ClassMemory:
		return true
	}
	return false
}

func (c *Controller) modeString() string {
	if c.fallbackActive {
		return "fallback"
	}
	return "manual"
}

// fanPctForPublish returns the last commanded fan percent, if known. It's
// deliberately unknown (ok=false) whenever c.commandedPct is -1, which is
// exactly the state fallback puts it in — so MQTT never reports a stale
// manual fan percent while the iDRAC is actually the one driving the fans.
func (c *Controller) fanPctForPublish() (float64, bool) {
	if c.commandedPct < 0 {
		return 0, false
	}
	return c.commandedPct, true
}

// buildSnapshot captures current sensor/group/fan state for MQTT
// publishing. It only reads controller state (no I/O), so it's cheap to
// call synchronously from tick() before handing the result to a goroutine
// for the actual (network) publish.
func (c *Controller) buildSnapshot() mqttpub.Snapshot {
	snap := mqttpub.Snapshot{Time: time.Now(), Mode: c.modeString(), Fallback: c.fallbackActive, FallbackReason: c.fallbackReason}
	if pct, ok := c.fanPctForPublish(); ok {
		snap.FanPercent, snap.HasFanPercent = pct, true
	}

	var tempSum float64
	var tempCount int
	snap.Sensors = make([]mqttpub.SensorReading, 0, len(c.cfg.Sensors))
	for _, s := range c.cfg.Sensors {
		if s.Disabled {
			continue
		}
		st := c.states[s.ID]
		r := mqttpub.SensorReading{ID: s.ID, Stale: st.staleTicks > 0}
		if st.emaInit {
			r.Value, r.HasValue = st.ema, true
			if isTempClass(s.Class) {
				tempSum += st.ema
				tempCount++
			}
		}
		snap.Sensors = append(snap.Sensors, r)
	}
	if tempCount > 0 {
		snap.AvgTemp, snap.HasAvgTemp = tempSum/float64(tempCount), true
	}

	snap.Groups = make([]mqttpub.GroupReading, 0, len(c.cfg.Groups))
	for _, g := range c.cfg.Groups {
		if !g.Enabled {
			continue
		}
		agg, ok := c.aggregate(g)
		snap.Groups = append(snap.Groups, mqttpub.GroupReading{Name: g.Name, Value: agg, HasValue: ok})
	}
	return snap
}

// publishAsync fires the MQTT snapshot publish in its own goroutine so a
// slow or unreachable broker can never delay the next tick. It's tracked
// via mqttWG so Run can wait for it to finish before disconnecting on
// shutdown, instead of racing a still-in-flight publish.
func (c *Controller) publishAsync() {
	if c.mqttPub == nil {
		return
	}
	snap := c.buildSnapshot()
	c.mqttWG.Add(1)
	go func() {
		defer c.mqttWG.Done()
		c.mqttPub.PublishSnapshot(snap)
	}()
}

// runHookAsync fires cmdline (via `/bin/sh -c`) in its own goroutine, never
// blocking the caller (the tick that just decided to engage/clear the
// safety fallback) regardless of how slow or broken the command is.
// Deliberately runs in dry-run too: a notification hook has no effect on
// hardware, and dry-run is exactly when someone would want to safely test
// it fires correctly.
func (c *Controller) runHookAsync(event, cmdline, reason string) {
	if cmdline == "" {
		return
	}
	node, _ := os.Hostname()
	if node == "" {
		node = "dellfanctl"
	}
	env := append(os.Environ(),
		"DELLFANCTL_EVENT="+event,
		"DELLFANCTL_NODE="+node,
		"DELLFANCTL_TIME="+time.Now().Format(time.RFC3339),
	)
	if reason != "" {
		env = append(env, "DELLFANCTL_REASON="+reason)
	}

	c.hookWG.Add(1)
	go func() {
		defer c.hookWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			c.log.Warn("notification hook failed", "event", event, "error", err, "output", string(out))
			return
		}
		c.log.Info("notification hook ran", "event", event)
	}()
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
	// Runs after every branch below, however it exits, so MQTT always gets
	// a fresh snapshot reflecting whatever this tick actually decided.
	defer c.publishAsync()

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
			c.fallbackReason = reason
			c.commandedPct = -1
			c.goodTicks = 0
			c.runHookAsync("fallback", c.cfg.Safety.OnFallbackCmd, reason)
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
				c.runHookAsync("recover", c.cfg.Safety.OnRecoverCmd, "")
				c.fallbackReason = ""
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
