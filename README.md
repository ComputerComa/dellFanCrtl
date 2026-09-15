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

## Quick install

Every tagged release (`.github/workflows/release.yml`) publishes a single
`linux/amd64` binary plus a `.sha256` checksum file as GitHub Release
assets. `install.sh` downloads the latest release, verifies the binary
against its checksum before touching anything, installs `ipmitool` and
`smartmontools`, installs the binary to `/usr/local/bin/dellfanctl`, runs
`dellfanctl discover` to generate `/etc/dellfanctl/config.yaml`, and writes
the systemd unit — but does **not** enable or start the service, so you can
review the generated config and dry-run it first (see [Safety
notes](#safety-notes)).

```sh
curl -fsSL https://raw.githubusercontent.com/ComputerComa/dellFanCrtl/master/install.sh | sudo bash
```

As with any script piped into a root shell, consider downloading and
reading it first instead:

```sh
curl -fsSLO https://raw.githubusercontent.com/ComputerComa/dellFanCrtl/master/install.sh
less install.sh
sudo bash install.sh
```

Run `sudo bash install.sh --help` for flags (pin a specific `--version`,
`--skip-packages`, `--skip-discover`, `--enable` to start the service
immediately instead of waiting for you to review it, etc).

`install.sh` assumes a regular Linux host with a writable `/`, a package
manager, and systemd — that doesn't describe every NAS OS. **On TrueNAS
SCALE, use the container image instead** (its root filesystem is replaced
on every update, so a plain binary + systemd unit installed by the script
above wouldn't survive one) — see
[`deploy/truenas/README.md`](deploy/truenas/README.md).

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

### Hardware changes (drive swaps, new disks)

Disk sensors are matched by controller slot (`device` + `device_type`,
e.g. `megaraid,3`), not serial number — so **replacing a drive in the same
bay needs no config change at all**: the sensor just starts reading
whatever's physically in that slot now. This is deliberate and matches
how the tool is meant to be operated: config is reviewed once, by a human,
and stays static until you deliberately change it — an unreviewed change
to what drives the fan curve (a new drive silently added with
auto-guessed thresholds, or a vanished one silently dropped from an
average) is exactly the kind of surprise this tool exists to avoid, not
introduce. If a `required` drive sensor *can't* be read at all (removed,
failed, or — the case that actually prompted this section — an entire
array rebuilt onto new hardware), that's treated like any other required
sensor going stale: the safety fallback engages (see below), which is the
correct response to "I can no longer verify this is safe," not a bug.

To find out about drive changes on your own terms instead of via the fans
spinning up:

```sh
dellfanctl discover --config /etc/dellfanctl/config.yaml --diff
```

Re-probes hardware and reports drift against the current config — sensors
it configured but can no longer confirm, and anything new it found that
isn't monitored yet — without changing anything. Exits `2` if it found
drift (`0` if clean), so it's cron/monitoring-friendly. After planned
maintenance (added/removed/rebuilt an array), review what it reports and
either hand-edit the config for the specific sensors that changed
(keeps your tuned curves/thresholds), or re-run `discover --force` to
regenerate everything from scratch (simpler for a full rebuild, but
discards any hand-tuning — review the new file just like the first time).

## MQTT / Home Assistant

Set `mqtt.enabled: true` in the config (see `config.example.yaml`) to publish
every sensor and group reading, plus two summary entities, to an MQTT
broker with Home Assistant MQTT discovery — entities appear automatically
under one device, no HA-side YAML needed.

- **Non-blocking**: the publisher connects with paho's `ConnectRetry`/
  `AutoReconnect`, so a broker that's down or slow never delays startup or a
  tick. Each tick's publish runs in its own goroutine
  (`internal/control`'s `publishAsync`), so even a slow broker can't stall
  sensor polling or fan control — the 5s control loop cadence is
  unaffected either way.
- **Grouped by type**: individual sensors are published under
  `sensors/<class>/<id>` (cpu/disk/fan_rpm/inlet/exhaust/board), and each
  config `groups` entry (e.g. the disk average, the CPU max) gets its own
  entity under `groups/<name>` — so you see both each drive's temperature
  and the single averaged value actually driving the fan curve.
- **Overall summary entities**: `summary/fan_percent` (the current
  commanded fan speed) and `summary/avg_temp` (the average of every live
  temperature sensor), plus `summary/mode` (`manual` or `fallback`) and
  `summary/last_update` (a `device_class: timestamp` entity).
- **`summary/fallback`** is a dedicated `binary_sensor` with
  `device_class: problem` (not just text buried in `summary/mode`), so
  Home Assistant offers a one-click "notify me" automation
  ("Device became problem") instead of everyone having to hand-build a
  template trigger — the whole point being you find out *before* you
  notice the fans, not after. Its `json_attributes_topic` carries `reason`
  (why `safetyTrip()` fired, e.g. which sensor went stale). For a non-HA
  notification path (or as well as), see `safety.on_fallback_cmd` /
  `on_recover_cmd` in `config.example.yaml` — runs any command/webhook you
  want the moment fallback engages or clears.
- **Retained + freshness**: every state is published retained, so Home
  Assistant has a value immediately on restart. Staleness is covered two
  ways: an MQTT availability topic (`<base>/status`, backed by a Last Will)
  flips to `offline` the moment the process dies or is stopped — cleanly on
  shutdown, or via the broker's own LWT detection if it's killed — so HA
  greys out the entities instead of showing a frozen number forever; and
  every sensor/group also carries a `json_attributes_topic` with
  `last_updated` (plus `stale` for individual sensors) for finer-grained
  checks.
- While in fallback (iDRAC automatic control), `summary/fan_percent` is
  deliberately left unpublished that tick rather than showing a stale
  manual-mode number, since dellfanctl no longer knows what speed the
  iDRAC has actually chosen.

Credentials: prefer `mqtt.password_env` (an environment variable name) over
putting a plaintext password in the config file. Either way, the MQTT
username should be a broker account scoped to only what this needs.

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
- **The fallback is silent unless you wire up something to notice it.**
  100% fan noise at 9pm because a required sensor went stale is the
  fallback working correctly, not a malfunction — but finding out an hour
  later because the fans were loud isn't good enough. Set
  `safety.on_fallback_cmd` (any command/webhook — ntfy, Pushover, `wall`,
  whatever) and/or watch the MQTT `summary/fallback` problem entity — see
  MQTT / Home Assistant above.

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
internal/mqttpub/     non-blocking MQTT publisher + Home Assistant discovery
internal/version/     shared version string
config.example.yaml   annotated example config
deploy/               systemd unit template
deploy/truenas/       TrueNAS SCALE container deployment (see below)
install.sh            curl-pipeable installer (see Quick install above)
Dockerfile            container image (used by deploy/truenas/, GHCR release)
.github/workflows/    release workflow (builds + publishes the binary and image)
```

## TrueNAS SCALE / Docker

`ghcr.io/computercoma/dellfanctl` is published alongside every tagged
release, built from the [`Dockerfile`](Dockerfile) in this repo (same
binary, `ipmitool` + `smartmontools` baked in). This is the recommended
way to run dellfanctl on TrueNAS SCALE or any other host where you'd
rather not install a binary/systemd unit directly onto the OS — see
[`deploy/truenas/README.md`](deploy/truenas/README.md) for the full
walkthrough (create a dataset, run discovery, review the config, then
deploy either as a [Custom
App](deploy/truenas/docker-compose.yaml) via pasted YAML, or as a
[catalog app](deploy/truenas/catalog/README.md) with a generated settings
form like any built-in SCALE app — see that page for how proven each path
is).
