# Running dellfanctl on TrueNAS SCALE

`install.sh` (see the repo root README) is written for a regular Linux
host with a normal writable `/`, a package manager, and systemd. None of
that holds up on TrueNAS SCALE: the boot/root filesystem is replaced
wholesale on every TrueNAS update, so a binary dropped in
`/usr/local/bin` and a hand-written unit in `/etc/systemd/system` would
both disappear the next time SCALE updates itself, and `apt-get install`
against the appliance's base OS isn't reliable or supported either.

Instead, dellfanctl ships as a container image
(`ghcr.io/computercoma/dellfanctl`) that you run through SCALE's own
**Apps** system, which stores everything on a ZFS pool rather than the
boot environment — it survives TrueNAS updates the same way any other
app does.

## 1. Create a dataset for the config

In the TrueNAS UI: **Datasets** → your pool → **Add Dataset** → e.g.
`apps/dellfanctl/config`. This is where the generated `config.yaml`
will live, independent of the container.

## 2. Run discovery once

Discovery just probes IPMI/SMART and writes a config file — it doesn't
touch fan speed. Run it as a one-off container against the dataset you
just created (from the TrueNAS shell, System Settings → Shell, or SSH):

```sh
docker run --rm -it --privileged \
  -v /mnt/YOUR_POOL/apps/dellfanctl/config:/etc/dellfanctl \
  ghcr.io/computercoma/dellfanctl:latest \
  discover --config /etc/dellfanctl/config.yaml
```

Then review `/mnt/YOUR_POOL/apps/dellfanctl/config/config.yaml` from the
TrueNAS UI's file browser or shell: check sensor classifications, curves,
and `warn_c`/`crit_c` thresholds, same as any other install (see the main
README's "Config file" section).

## 3. Dry-run and a single real tick

Same safety steps as a normal install, just via `docker run` instead of
`sudo dellfanctl`:

```sh
# Logs actions, touches no hardware:
docker run --rm --privileged \
  -v /mnt/YOUR_POOL/apps/dellfanctl/config:/etc/dellfanctl \
  ghcr.io/computercoma/dellfanctl:latest \
  run --config /etc/dellfanctl/config.yaml --dry-run

# One real tick, to sanity-check actual fan response:
docker run --rm --privileged \
  -v /mnt/YOUR_POOL/apps/dellfanctl/config:/etc/dellfanctl \
  ghcr.io/computercoma/dellfanctl:latest \
  run --config /etc/dellfanctl/config.yaml --once
```

## 4. Deploy as a persistent app

Once you're happy with the config, there are two ways to actually deploy
it - pick one:

- **Custom App (proven, works today):** **Apps** → **Discover Apps** →
  **Custom App** (labelled "Install via YAML" on some SCALE versions) and
  paste in [`docker-compose.yaml`](docker-compose.yaml) from this
  directory, editing the volume path to match your dataset. Start it.
- **Catalog app (a generated settings form, like any built-in app):** add
  this repo as a custom catalog and install "dellfanctl" from Discover
  Apps instead of pasting YAML - see
  [`catalog/README.md`](catalog/README.md) for how to add it and an
  important caveat: it's built to mirror real, working TrueNAS apps and
  its template has been rendered against TrueNAS's own library locally,
  but hasn't been through an actual TrueNAS catalog sync yet. If it
  doesn't work, the Custom App above is the fallback.

Either way, `restart: unless-stopped` plays the role systemd's
`Restart=on-failure` normally would; `stop_grace_period: 15s` gives it
the same room the systemd unit gets (`TimeoutStopSec=15`) to hand fan
control back to the iDRAC on shutdown/restart. Logs are visible from the
app's **Logs** tab in the Apps UI, or `docker logs dellfanctl`.

## Updating

Pull a newer image tag (e.g. a specific release instead of `:latest`) and
redeploy the app from the Apps UI, or from the shell:

```sh
docker pull ghcr.io/computercoma/dellfanctl:latest
docker compose -f docker-compose.yaml up -d
```

Config on the dataset is untouched by an image update — it's only
regenerated if you re-run `discover --force`.

## About `--privileged`

The example compose file runs the container `--privileged` so it can
reach `/dev/ipmi0` and whatever device nodes smartctl needs to see disks
behind your storage controller (`/dev/bus/*`, `/dev/sg*`, ...) — these
vary by board, so `--privileged` is the "just get it working" default.
Once you know exactly which device nodes your hardware needs, you can
replace `privileged: true` with explicit `devices:` entries for those
paths (and drop `privileged`) for a narrower grant.
