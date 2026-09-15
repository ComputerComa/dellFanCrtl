# dellfanctl as a TrueNAS SCALE catalog app

This directory is a self-hosted TrueNAS SCALE app catalog containing a
single app: `dellfanctl`, in the `community` train. Adding it as a custom
catalog gets you the same "Discover Apps" experience as any built-in app
(a generated settings form, Start/Stop/Logs in the Apps UI) instead of
pasting YAML by hand.

**This has not been installed on a real TrueNAS box** - it was built by
mirroring the exact structure and library version of real, currently
shipping community apps (`zerotier`, `scrutiny`) from
[truenas/apps](https://github.com/truenas/apps), and the
`templates/docker-compose.yaml` Jinja2 template was actually rendered
locally against TrueNAS's own vendored rendering library (not just
hand-written and hoped-for) - see "How this was validated" below. But the
app_versions.json health/version-index format is a best-effort
reconstruction of a file iXsystems' own tooling normally generates, and
none of it has been through a live TrueNAS catalog sync. If it doesn't
show up or fails to install, the [Custom App](../docker-compose.yaml)
path in the parent directory is the proven-working fallback - please open
an issue either way so this can be fixed.

## Installing

1. In the TrueNAS UI: **Apps** → **Discover Apps** → **Manage Catalogs** →
   **Add Catalog**.
2. Repository: `https://github.com/ComputerComa/dellFanCrtl`
3. Branch: `master`
4. Train: `community`
5. Save, then find **dellfanctl** under Discover Apps.

Before installing, create a dataset for its config and run `dellfanctl
discover` against it exactly as described in
[`../README.md`](../README.md) steps 1-3 - the app only *runs* dellfanctl
(`run --config /etc/dellfanctl/config.yaml`), it doesn't generate the
config for you. Point the app's "dellfanctl Config Storage" (Host Path)
at that same dataset when installing.

## What the app does

- Runs the container `--privileged`, with `/dev` bind-mounted read-write
  and `/run/udev` read-only - same reasoning as the plain [Custom
  App](../docker-compose.yaml): the exact device nodes needed for
  `/dev/ipmi0` and SMART/RAID controller access vary by board and can't
  be enumerated in a settings form.
- `stop_grace_period: 15s`, matching the systemd unit's
  `TimeoutStopSec=15`, so a stop/restart gives dellfanctl time to hand
  fan control back to the iDRAC.
- Healthcheck is a plain `pgrep -f dellfanctl` (no network service to
  probe against).
- Timezone (for log timestamps) is the only app-specific setting exposed
  in the form; everything else dellfanctl-specific lives in
  `config.yaml` on the mounted dataset, same as every other deployment
  method in this repo.

## How this was validated

Docker isn't available in the environment this was built in, so the full
`docker compose up` path is untested. What *was* done: the real
`truenas/apps` repo was cloned, and `templates/docker-compose.yaml` here
was rendered directly against the actual vendored `base_v2_3_4` library
copied into `1.0.0/templates/library/` (the same one `zerotier` and
`scrutiny` ship) using a small local Jinja2 harness - not their full
`ghcr.io/truenas/apps_validation` CI container (which needs Docker), but
the same Python rendering code that container runs. That caught two real
bugs before they'd have surfaced on an actual NAS: a required healthcheck
that wasn't set, and a manual `TZ` env var that collided with one the
library injects automatically. The final render produced a complete,
sensible `docker-compose` service definition (privileged, the intended
capabilities and volumes, `restart: unless-stopped`, the healthcheck,
`platform: linux/amd64`).

## Updating for a new dellfanctl release

Bumping the container image tag (`ghcr.io/computercoma/dellfanctl:vX.Y.Z`)
without changing the compose logic:

1. Update `app_version` in `trains/community/dellfanctl/1.0.0/app.yaml`
   and `tag` in the same directory's `ix_values.yaml`.
2. Update the matching fields in
   `trains/community/dellfanctl/app_versions.json`
   (`app_metadata.app_version`, `human_version`).

A change to `questions.yaml`, `templates/docker-compose.yaml`, or
anything else user-facing should instead bump `version` in `app.yaml`,
add a new `trains/community/dellfanctl/<new-version>/` directory (TrueNAS
keeps old versions installable), and add an entry for it in
`app_versions.json`, following the pattern
[truenas/apps](https://github.com/truenas/apps) itself uses for every
app's version history.
