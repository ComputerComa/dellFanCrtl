#!/usr/bin/env bash
# install.sh - download, verify, and install dellfanctl.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ComputerComa/dellFanCrtl/master/install.sh | sudo bash
#
# or download it first and inspect it (recommended before piping any script
# into a root shell):
#   curl -fsSLO https://raw.githubusercontent.com/ComputerComa/dellFanCrtl/master/install.sh
#   less install.sh
#   sudo bash install.sh [flags]
#
# What this does, in order:
#   1. Downloads the latest (or --version-specified) release binary and its
#      .sha256 checksum file from GitHub Releases, and verifies the binary
#      against that checksum before doing anything else with it.
#   2. Installs the OS packages dellfanctl needs (ipmitool, smartmontools).
#   3. Installs the binary to /usr/local/bin/dellfanctl.
#   4. Runs `dellfanctl discover` to generate /etc/dellfanctl/config.yaml
#      from your hardware's actual sensors, unless a config already exists.
#   5. Runs `dellfanctl install-service` to write a systemd unit.
#
# It deliberately does NOT enable or start the service by default -
# review the generated config first (see the safety notes it prints, and
# README.md). Pass --enable if you want it started immediately.
set -euo pipefail

REPO="ComputerComa/dellFanCrtl"
BINARY_NAME="dellfanctl-linux-amd64"
INSTALL_PATH="/usr/local/bin/dellfanctl"
CONFIG_PATH="/etc/dellfanctl/config.yaml"
UNIT_PATH="/etc/systemd/system/dellfanctl.service"
VERSION=""              # empty = latest release
SKIP_PACKAGES=0
SKIP_DISCOVER=0
FORCE_DISCOVER=0
FORCE_UNIT=0
ENABLE_NOW=0
ASSUME_YES=0

# --- output helpers ---------------------------------------------------
if [ -t 1 ]; then
  c_red='\033[31m'; c_yellow='\033[33m'; c_green='\033[32m'; c_bold='\033[1m'; c_reset='\033[0m'
else
  c_red=''; c_yellow=''; c_green=''; c_bold=''; c_reset=''
fi
info()  { printf '%b\n' "${c_green}==>${c_reset} $*"; }
warn()  { printf '%b\n' "${c_yellow}warning:${c_reset} $*" >&2; }
die()   { printf '%b\n' "${c_red}error:${c_reset} $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
install.sh [flags]

  --version vX.Y.Z     install this release instead of the latest
  --config PATH        config path to generate/use (default: /etc/dellfanctl/config.yaml)
  --skip-packages       don't install ipmitool/smartmontools
  --skip-discover       don't run `dellfanctl discover`
  --force-discover      overwrite an existing config with a freshly discovered one
  --force-unit          overwrite an existing systemd unit file
  --enable               after installing, run `systemctl enable --now dellfanctl`
                          (skipped by default - review the config first!)
  -y, --yes             don't pause for confirmation before installing
  -h, --help             show this help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --config) CONFIG_PATH="$2"; shift 2 ;;
    --skip-packages) SKIP_PACKAGES=1; shift ;;
    --skip-discover) SKIP_DISCOVER=1; shift ;;
    --force-discover) FORCE_DISCOVER=1; shift ;;
    --force-unit) FORCE_UNIT=1; shift ;;
    --enable) ENABLE_NOW=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown flag: $1 (see --help)" ;;
  esac
done

# --- sanity checks ------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || die "dellfanctl only runs on Linux (talks directly to /dev/ipmi0)"

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) : ;;
  *) die "no release binary for architecture '$arch' - Dell PowerEdge/iDRAC hardware is x86_64; if you're cross-compiling for something else, build from source instead (see README.md)" ;;
esac

if [ "$(id -u)" -ne 0 ]; then
  die "must be run as root (installs packages, a systemd unit, and writes under /etc). Re-run with sudo."
fi

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found"; }
need_cmd curl
need_cmd sha256sum
need_cmd install

if [ "$ASSUME_YES" -ne 1 ]; then
  printf '%b' "${c_bold}This will download and install dellfanctl"
  [ -n "$VERSION" ] && printf ' %s' "$VERSION"
  printf " to ${INSTALL_PATH}, install ipmitool/smartmontools, and write a\nsystemd unit at ${UNIT_PATH}. Continue? [y/N] ${c_reset}"
  read -r reply </dev/tty || reply="n"
  case "$reply" in
    y|Y|yes|YES) ;;
    *) die "aborted" ;;
  esac
fi

# --- fetch release metadata ---------------------------------------------
gh_curl() {
  local url="$1"
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" "$url"
  else
    curl -fsSL "$url"
  fi
}

if [ -n "$VERSION" ]; then
  api_url="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
else
  api_url="https://api.github.com/repos/${REPO}/releases/latest"
fi

info "Looking up release metadata (${VERSION:-latest})..."
release_json="$(gh_curl "$api_url")" || die "failed to reach GitHub API at $api_url"

extract_asset_url() {
  # Pull the browser_download_url for an asset whose name ends with $1 out
  # of the release JSON. Avoids a hard dependency on jq.
  printf '%s\n' "$release_json" \
    | grep -Eo '"browser_download_url" *: *"[^"]+"' \
    | sed -E 's/.*"(https[^"]+)"$/\1/' \
    | grep -E "/${1}\$" \
    | head -n1
}

resolved_tag="$(printf '%s\n' "$release_json" | grep -m1 -Eo '"tag_name" *: *"[^"]+"' | sed -E 's/.*"([^"]+)"$/\1/')"
[ -n "$resolved_tag" ] || die "couldn't find a release${VERSION:+ tagged $VERSION} for ${REPO} (check --version, or that a release workflow has run)"

bin_url="$(extract_asset_url "${BINARY_NAME}")"
sha_url="$(extract_asset_url "${BINARY_NAME}\\.sha256")"
[ -n "$bin_url" ] || die "release ${resolved_tag} has no ${BINARY_NAME} asset"
[ -n "$sha_url" ] || die "release ${resolved_tag} has no ${BINARY_NAME}.sha256 asset"

info "Installing dellfanctl ${resolved_tag}"

# --- download + verify ---------------------------------------------------
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

info "Downloading ${BINARY_NAME}..."
curl -fsSL -o "${tmpdir}/${BINARY_NAME}" "$bin_url"
curl -fsSL -o "${tmpdir}/${BINARY_NAME}.sha256" "$sha_url"

info "Verifying checksum..."
( cd "$tmpdir" && sha256sum -c "${BINARY_NAME}.sha256" ) \
  || die "checksum verification FAILED - downloaded binary does not match the published .sha256. Not installing."

chmod 0755 "${tmpdir}/${BINARY_NAME}"

# --- install prerequisite packages ---------------------------------------
if [ "$SKIP_PACKAGES" -eq 1 ]; then
  info "Skipping package installation (--skip-packages)"
else
  info "Installing prerequisite packages (ipmitool, smartmontools)..."
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y
    apt-get install -y ipmitool smartmontools
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ipmitool smartmontools
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ipmitool smartmontools
  elif command -v zypper >/dev/null 2>&1; then
    zypper --non-interactive install ipmitool smartmontools
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm ipmitool smartmontools
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ipmitool smartmontools
  else
    warn "couldn't detect a supported package manager (apt/dnf/yum/zypper/pacman/apk)."
    warn "install ipmitool and smartmontools manually before running dellfanctl."
  fi
fi

# --- install the binary ---------------------------------------------------
info "Installing ${INSTALL_PATH}..."
install -m 0755 -o root -g root "${tmpdir}/${BINARY_NAME}" "$INSTALL_PATH"
"$INSTALL_PATH" version

# --- discover sensors / generate config ------------------------------------
if [ "$SKIP_DISCOVER" -eq 1 ]; then
  info "Skipping sensor discovery (--skip-discover)"
elif [ -f "$CONFIG_PATH" ] && [ "$FORCE_DISCOVER" -ne 1 ]; then
  info "Config already exists at ${CONFIG_PATH}, leaving it alone (pass --force-discover to regenerate)"
else
  info "Running sensor discovery (probes ipmitool/smartctl, changes no hardware state)..."
  mkdir -p "$(dirname "$CONFIG_PATH")"
  discover_args=(discover --config "$CONFIG_PATH")
  [ "$FORCE_DISCOVER" -eq 1 ] && discover_args+=(--force)
  if ! "$INSTALL_PATH" "${discover_args[@]}"; then
    warn "discovery failed - you can re-run it manually later with:"
    warn "  sudo dellfanctl discover --config ${CONFIG_PATH}"
  fi
fi

# --- install the systemd unit ----------------------------------------------
info "Writing systemd unit at ${UNIT_PATH}..."
unit_args=(install-service --config "$CONFIG_PATH" --unit-path "$UNIT_PATH")
[ "$FORCE_UNIT" -eq 1 ] && unit_args+=(--force)
if [ -f "$UNIT_PATH" ] && [ "$FORCE_UNIT" -ne 1 ]; then
  info "Unit already exists at ${UNIT_PATH}, leaving it alone (pass --force-unit to overwrite)"
else
  "$INSTALL_PATH" "${unit_args[@]}"
  systemctl daemon-reload
fi

# --- enable/start, or explain how to -----------------------------------
if [ "$ENABLE_NOW" -eq 1 ]; then
  info "Enabling and starting dellfanctl now (--enable was passed)..."
  systemctl enable --now dellfanctl
  info "Started. Follow logs with: journalctl -u dellfanctl -f"
else
  cat <<EOF

${c_bold}dellfanctl ${resolved_tag} is installed but NOT started.${c_reset}

Before going further:
  1. Review the generated config: ${CONFIG_PATH}
     Check sensor classifications, curves, and warn_c/crit_c thresholds.
  2. Dry-run it (logs actions, touches no hardware):
     sudo dellfanctl run --config ${CONFIG_PATH} --dry-run
  3. One real tick, to sanity-check actual fan response:
     sudo dellfanctl run --config ${CONFIG_PATH} --once
  4. Once you're happy, start the service:
     sudo systemctl enable --now dellfanctl
     journalctl -u dellfanctl -f

(Re-run this script with --enable to skip straight to step 4 next time.)
EOF
fi
