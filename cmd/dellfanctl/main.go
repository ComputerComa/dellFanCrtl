// Command dellfanctl discovers IPMI/SMART sensors on a Dell PowerEdge
// server, generates a fan-control config from them, and can run as a
// service that puts the iDRAC into manual fan control and drives fan speed
// from a smoothed, curve-based control loop with a safety fallback back to
// automatic control.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dellfanctl/internal/control"
	"dellfanctl/internal/discover"
	"dellfanctl/internal/ipmi"
	"dellfanctl/internal/model"
	"dellfanctl/internal/mqttpub"
	"dellfanctl/internal/version"
)

const defaultConfigPath = "/etc/dellfanctl/config.yaml"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "discover":
		err = cmdDiscover(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "revert":
		err = cmdRevert(os.Args[2:])
	case "set-speed":
		err = cmdSetSpeed(os.Args[2:])
	case "install-service":
		err = cmdInstallService(os.Args[2:])
	case "version":
		fmt.Println("dellfanctl " + version.Version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dellfanctl - discover sensors and control Dell iDRAC fan speed via ipmitool

Usage:
  dellfanctl discover [flags]        probe sensors and write a config file
  dellfanctl run [flags]             run the fan control loop (the "auto mode")
  dellfanctl revert [flags]          immediately hand fan control back to the iDRAC
  dellfanctl set-speed <pct> [flags] set a fixed manual fan speed once, for testing
  dellfanctl install-service [flags] write a systemd unit file
  dellfanctl version

Run 'dellfanctl <command> -h' for flags on a given command.

Always test a new config with 'dellfanctl run --dry-run' before running for
real, and keep physical access to the machine in case fans need a manual
BIOS/iDRAC reset.
`)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to write the generated config")
	force := fs.Bool("force", false, "overwrite an existing config file")
	skipIPMI := fs.Bool("skip-ipmi", false, "skip IPMI sensor discovery")
	skipSMART := fs.Bool("skip-smart", false, "skip SMART disk discovery")
	ipmitoolPath := fs.String("ipmitool", "ipmitool", "path to the ipmitool binary")
	smartctlPath := fs.String("smartctl", "smartctl", "path to the smartctl binary")
	diff := fs.Bool("diff", false, "report drift against the existing config instead of writing one (exit 2 if any found)")
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	opts := discover.Options{
		IpmitoolPath: *ipmitoolPath,
		SmartctlPath: *smartctlPath,
		SkipIPMI:     *skipIPMI,
		SkipSMART:    *skipSMART,
		Logf:         func(f string, a ...interface{}) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}

	if *diff {
		return cmdDiscoverDiff(ctx, *configPath, opts)
	}

	cfg, err := discover.Run(ctx, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(*configPath), 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := model.Save(*configPath, cfg, *force); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s with %d sensors and %d groups (%d enabled).\n",
		*configPath, len(cfg.Sensors), len(cfg.Groups), countEnabled(cfg))
	fmt.Println("Review it, then test with: dellfanctl run --config", *configPath, "--dry-run")
	return nil
}

// cmdDiscoverDiff implements 'discover --diff': re-probe hardware and
// report drift against the already-reviewed config at configPath, without
// touching it. Lets an operator see "your drives changed" as a deliberate
// check (e.g. before/after planned maintenance, or from cron) instead of
// finding out only once a required sensor has already gone stale long
// enough to trip the safety fallback.
func cmdDiscoverDiff(ctx context.Context, configPath string, opts discover.Options) error {
	old, err := model.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading existing config (nothing to diff against): %w", err)
	}
	fresh, err := discover.Run(ctx, opts)
	if err != nil {
		return err
	}

	res := discover.Diff(old, fresh)
	if !res.HasChanges() {
		fmt.Println("\nNo drift: every currently configured sensor is still readable, and the probe found nothing new.")
		return nil
	}

	fmt.Println()
	if len(res.Missing) > 0 {
		fmt.Println("MISSING - configured, but this probe couldn't confirm them:")
		for _, s := range res.Missing {
			fmt.Printf("  - %-16s %-30s %s\n", s.ID, s.Name, sensorAddr(s))
			if s.Required {
				fmt.Println("      REQUIRED: going/staying stale trips the safety fallback to iDRAC automatic control.")
			}
		}
		fmt.Println()
	}
	if len(res.New) > 0 {
		fmt.Println("NEW - found by this probe, not in the current config (not monitored, doesn't affect the fan curve):")
		for _, s := range res.New {
			fmt.Printf("  - %-30s class=%-6s %s\n", s.Name, s.Class, sensorAddr(s))
		}
		fmt.Println()
	}
	fmt.Println("To pick these up: hand-edit", configPath, "(keeps your tuned curves/thresholds - safest",
		"for a like-for-like drive swap, which often needs no change at all), or re-run")
	fmt.Println("  dellfanctl discover --config", configPath, "--force")
	fmt.Println("to fully regenerate it (this REPLACES the whole file, including any curves/thresholds")
	fmt.Println("you've hand-tuned since - review the new file just like the first time).")

	os.Exit(2) // distinct from the generic os.Exit(1) on error, so e.g. a cron job can tell "drift found" from "probe failed"
	return nil
}

func sensorAddr(s model.Sensor) string {
	if s.Source == model.SourceSMART {
		return fmt.Sprintf("device=%s device_type=%s", s.Device, s.DeviceType)
	}
	return fmt.Sprintf("ipmi_name=%q occurrence=%d", s.IPMIName, s.Occurrence)
}

func countEnabled(cfg *model.Config) int {
	n := 0
	for _, g := range cfg.Groups {
		if g.Enabled {
			n++
		}
	}
	return n
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	dryRun := fs.Bool("dry-run", false, "log actions without executing any ipmitool commands")
	once := fs.Bool("once", false, "run a single poll/decide/apply cycle and exit, instead of looping")
	fs.Parse(args)

	cfg, err := model.Load(*configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Logging.Level)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := control.New(cfg, log, *dryRun)
	if cfg.MQTT.Enabled {
		pub := mqttpub.New(cfg.MQTT, log)
		if err := pub.Start(); err != nil {
			return fmt.Errorf("starting mqtt publisher: %w", err)
		}
		c.SetMQTT(pub)
	}
	return c.Run(ctx, *once)
}

func cmdRevert(args []string) error {
	fs := flag.NewFlagSet("revert", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	ipmitoolPath := fs.String("ipmitool", "", "path to ipmitool (overrides config)")
	fs.Parse(args)

	dell := model.Default().Dell
	bin := "ipmitool"
	if cfg, err := model.Load(*configPath); err == nil {
		dell = cfg.Dell
		bin = cfg.IpmitoolPath
	}
	if *ipmitoolPath != "" {
		bin = *ipmitoolPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ipmi.SetAutoMode(ctx, bin, dell); err != nil {
		return err
	}
	fmt.Println("fan control returned to iDRAC automatic mode")
	return nil
}

func cmdSetSpeed(args []string) error {
	fs := flag.NewFlagSet("set-speed", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	yes := fs.Bool("yes", false, "required: confirms you want to directly set a manual fan speed")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dellfanctl set-speed <percent> --yes")
	}
	if !*yes {
		return fmt.Errorf("refusing to set fan speed without --yes (this immediately changes real fan speed)")
	}
	var pct int
	if _, err := fmt.Sscanf(fs.Arg(0), "%d", &pct); err != nil {
		return fmt.Errorf("invalid percent %q", fs.Arg(0))
	}

	cfg, err := model.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ipmi.SetManualMode(ctx, cfg.IpmitoolPath, cfg.Dell); err != nil {
		return err
	}
	if err := ipmi.SetFanPercent(ctx, cfg.IpmitoolPath, cfg.Dell, pct); err != nil {
		return err
	}
	fmt.Printf("manual mode enabled, fan speed set to %d%%\n", pct)
	fmt.Println("remember: this stays in manual mode until you run 'dellfanctl revert' or start 'dellfanctl run'")
	return nil
}
