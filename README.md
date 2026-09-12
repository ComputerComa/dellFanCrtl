# dellfanctl

A small Go service that discovers the sensors on a Dell PowerEdge NAS
(IPMI + SMART), classifies them, and drives fan speed by talking directly to
the iDRAC's manual fan control interface via `ipmitool raw`. It replaces the
iDRAC's own (often noisy, coarse) automatic curve with one you control,
while keeping the iDRAC as a safety net if things go wrong.

## Why

The iDRAC's automatic fan curve is usually tuned conservatively (loud) and
doesn't know about drive temperatures behind a RAID/HBA controller, which it
can't see. `dellfanctl` reads both the IPMI sensor table (CPU, inlet,
exhaust, board, fan RPM) and SMART disk temperatures (including disks behind
a Dell PERC/MegaRAID controller), combines related sensors sensibly (e.g.
averaging all drive temperatures instead of reacting to every disk
individually), smooths out short-lived spikes, and ramps fan speed up and
down gradually instead of slamming to 100%.

## How it works

1. **`dellfanctl discover`** probes `ipmitool sensor list` / `sdr elist
   full` and `smartctl --scan` / disk queries, classifies every sensor
   (cpu, inlet, exhaust, board, disk, fan_rpm, ...), and writes a config
   file with sensible default groupings, curves, and safety thresholds.
2. You **review the generated config** (it's plain YAML) and adjust curves,
   thresholds, or which groups are enabled.
3. **`dellfanctl run`** puts the BMC into manual fan control and, every
   `poll_interval` (default 5s):
   - reads every configured sensor (IPMI in one batched call; SMART disks
     concurrently),
   - updates an exponential moving average (EMA) per sensor so a brief
     spike doesn't slam the fans,
   - aggregates each group (e.g. `avg` for disks, `max` for CPUs) and looks
     up the target fan percent on that group's curve,
   - takes the **max** across all enabled groups (the most demanding
     component wins),
   - ramps the commanded fan speed toward that target by at most
     `max_step_up_pct`/`max_step_down_pct` per tick, and holds steady
     before ramping *down* for `ramp_down_hold_ticks` ticks (so a brief dip
     doesn't cause hunting),
   - checks safety conditions every tick (see below) before applying
     anything.
4. If any required sensor crosses its critical threshold, a fan reports
   dangerously low RPM (possible fan failure), or sensors can't be read
   reliably, `dellfanctl` **immediately hands control back to the iDRAC's
   own automatic algorithm** and stays there until conditions recover for
   `recover_good_ticks` consecutive ticks.
5. On normal shutdown (Ctrl-C / `systemctl stop`) it also hands control
   back to the iDRAC before exiting, so a stopped daemon never leaves fans
   stuck at a stale manual speed.

## Building

```sh
go build -o dellfanctl ./cmd/dellfanctl
```

Requires `ipmitool` and (for disk temperatures) `smartctl` on the target
machine, and typically root (or an ipmitool/smartctl setup that doesn't
need it) to talk to `/dev/ipmi0` and the storage controller.

## Usage

```sh
# 1. Discover sensors and write a config (read-only, changes nothing).
sudo ./dellfanctl discover --config /etc/dellfanctl/config.yaml

# 2. Review /etc/dellfanctl/config.yaml: check classifications, curves,
#    and crit_c/warn_c thresholds. Disable groups you don't want driving
#    the fan curve.

# 3. ALWAYS dry-run first: logs every action without touching hardware.
sudo ./dellfanctl run --config /etc/dellfanctl/config.yaml --dry-run

# 4. A single real tick, to sanity-check actual fan response:
sudo ./dellfanctl run --config /etc/dellfanctl/config.yaml --once

# 5. Run for real (foreground; see install-service for a systemd unit).
sudo ./dellfanctl run --config /etc/dellfanctl/config.yaml
```

Escape hatches:

```sh
# Immediately hand fan control back to the iDRAC, no matter what state
# dellfanctl is in (or if it's not running at all).
sudo ./dellfanctl revert --config /etc/dellfanctl/config.yaml

# Force a specific manual fan speed once, for testing airflow/noise.
# Requires --yes since it changes real fan speed immediately.
sudo ./dellfanctl set-speed 40 --yes --config /etc/dellfanctl/config.yaml
```

### Running as a service

```sh
sudo ./dellfanctl install-service --config /etc/dellfanctl/config.yaml
sudo systemctl daemon-reload
sudo systemctl enable --now dellfanctl
journalctl -u dellfanctl -f
```

`install-service` only writes the unit file — it doesn't enable or start
anything, so you can review it first. See
`deploy/dellfanctl.service.example` for the template it's based on.

## Config file

See [`config.example.yaml`](config.example.yaml) for a fully-commented
example. Key things worth understanding:

- **Sensors** each have a `class` (cpu/inlet/exhaust/board/disk/fan_rpm/...)
  and a `source` (`ipmi` sensors are matched by exact name — plus
  `occurrence`, since some Dell boards report multiple sensors under the
  literal same name, e.g. two sensors both called `Temp` for CPU1/CPU2;
  `smart` sensors are matched by device path + smartctl `-d` argument,
  which is how disks behind a PERC/MegaRAID controller are addressed
  individually).
- **`required: true`** means: if this sensor goes unreadable for more than
  `safety.max_consecutive_errors` ticks, fall back to the iDRAC. Discovery
  marks every sensor that's part of an *enabled* group as required, plus
  all fan RPM sensors.
- **Groups** decide what actually drives the fan curve. `aggregation: avg`
  is used for disks (a fleet of drives should be averaged, not have every
  individual drive's temperature separately spike the fans); `max` is used
  for CPUs and board sensors (the hottest one should win). Only `enabled:
  true` groups affect fan speed; discovery leaves `inlet`/`exhaust` groups
  present but disabled by default, since ambient intake/exhaust
  temperature alone is a weak signal of actual component load — enable
  them if you want them factored in.
- **`dell.zones`**: `["0xff"]` (all zones at once) works on most 13G+
  PowerEdge boards. If fans don't respond, try one byte per physical fan
  instead, e.g. `["0x00","0x01","0x02","0x03","0x04","0x05"]`.
- **`safety.global_crit_c`** is the fallback ceiling used for any
  temperature sensor that doesn't have its own `crit_c` set. Discovery
  pre-fills each IPMI sensor's `crit_c`/`warn_c` from the BMC's own
  upper-critical/upper-non-critical thresholds where available.

## Safety notes

- This tool issues raw IPMI commands that bypass the iDRAC's own thermal
  protection logic while in manual mode. Bad configuration (or bad luck)
  really can overheat hardware. Always: review the generated config,
  `--dry-run` before running for real, and keep the safety thresholds
  (`crit_c`, `global_crit_c`, `min_rpm`) comfortably inside your hardware's
  actual limits, not at them.
- The manual-mode/set-speed/auto-mode raw commands (`0x30 0x30 ...`) are
  the widely-documented Dell PowerEdge OEM commands and are known to work
  across most 11G-15G generations, but BMC firmware varies. Test on a
  system you can physically get to.
- The safety fallback reverts to automatic control on: a sensor crossing
  its critical threshold, a fan reporting suspiciously low RPM, or
  sustained polling failures. It does **not** replace physically checking
  on a new deployment for the first while.

## Repository layout

```
cmd/dellfanctl/       CLI entry point and subcommands
internal/execx/       subprocess execution helper (timeouts, error capture)
internal/ipmi/        ipmitool sensor parsing + Dell OEM raw fan commands
internal/smart/       smartctl disk discovery + temperature reading
internal/classify/    sensor name/entity -> class heuristics
internal/model/       config file schema, defaults, validation, YAML I/O
internal/curve/       fan curve interpolation
internal/discover/    discovery orchestration -> generated config
internal/control/     the poll/smooth/ramp/fail-safe control loop
config.example.yaml   annotated example config
deploy/               systemd unit template
```
