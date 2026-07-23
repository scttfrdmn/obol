#!/bin/bash
# install-obol.sh — install the obol seam on an AWS ParallelCluster head node.
#
# Designed to run as an AWS ParallelCluster **HeadNode / CustomActions /
# OnNodeConfigured** bootstrap action (runs as root, after the node is fully
# configured — slurm.conf is written and slurmctld is up). It can also be run by
# hand over SSH on the head node (`sudo ./install-obol.sh`).
#
# It places the obol binaries + Lua seam + prolog/jobcomp scripts, writes the
# obold config + systemd unit, starts obold, and `scontrol reconfigure`s so
# slurmctld loads the job_submit plugin. The slurm.conf directives themselves
# (JobSubmitPlugins / PrologSlurmctld / JobCompType / JobCompLoc) come from the
# cluster YAML's CustomSlurmSettings — see cluster.sample.yaml — because
# ParallelCluster manages slurm.conf and would overwrite a hand-edit on update.
#
# Why PrologSlurmctld and not Prolog: ParallelCluster deny-lists Prolog and
# Epilog in CustomSlurmSettings (it uses them for its own node lifecycle).
# PrologSlurmctld is not deny-listed and runs on the head node — the controller
# where obold lives — so the BIND step attaches there instead. See
# docs/feasibility-parallelcluster.md.
#
# Arguments (all optional; also settable via the matching env var):
#   --release VERSION | OBOL_RELEASE   GitHub release tag to download (e.g. v0.14.0).
#                                      Default: fetch the latest release.
#   --bindir DIR      | OBOL_BINDIR    where obold/obol land (default /usr/local/bin)
#   --state-dir DIR   | OBOL_STATE_DIR obold state dir (default /var/lib/obol;
#                                      point at a shared FS mount to survive
#                                      head-node replacement — see the README)
#   --socket PATH     | OBOL_SOCKET    obold socket (default /run/obol/obold.sock)
#   --config PATH     | OBOL_CONFIG    obold.json to install (default: a starter
#                                      config written to /etc/obol/obold.json)
#   --source DIR      | OBOL_SOURCE    install the seam files from a local checkout
#                                      instead of downloading a release archive
#
# Idempotent: re-running updates the binaries/seam and restarts obold.

set -euo pipefail

log() { echo "[install-obol] $*"; }
die() { echo "[install-obol] ERROR: $*" >&2; exit 1; }

RELEASE="${OBOL_RELEASE:-}"
BINDIR="${OBOL_BINDIR:-/usr/local/bin}"
STATE_DIR="${OBOL_STATE_DIR:-/var/lib/obol}"
SOCKET="${OBOL_SOCKET:-/run/obol/obold.sock}"
CONFIG_SRC="${OBOL_CONFIG:-}"
SOURCE_DIR="${OBOL_SOURCE:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release)   RELEASE="$2"; shift 2 ;;
    --bindir)    BINDIR="$2"; shift 2 ;;
    --state-dir) STATE_DIR="$2"; shift 2 ;;
    --socket)    SOCKET="$2"; shift 2 ;;
    --config)    CONFIG_SRC="$2"; shift 2 ;;
    --source)    SOURCE_DIR="$2"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "must run as root (use sudo)"

# --- locate the slurm.conf directory (job_submit/lua loads job_submit.lua from
# the same dir as slurm.conf). ParallelCluster installs Slurm under /opt/slurm.
SLURM_ETC=""
if [[ -n "${SLURM_CONF:-}" && -f "${SLURM_CONF:-}" ]]; then
  SLURM_ETC="$(dirname "$SLURM_CONF")"
else
  for c in /opt/slurm/etc/slurm.conf /etc/slurm/slurm.conf; do
    [[ -f "$c" ]] && SLURM_ETC="$(dirname "$c")" && break
  done
fi
[[ -n "$SLURM_ETC" ]] || die "could not find slurm.conf (set SLURM_CONF)"
log "slurm config dir: $SLURM_ETC"

# The lua modules live in a fixed dir that matches job_submit.lua's OBOL_LUA_DIR
# default, so no per-node env is needed for the embedded slurmctld interpreter.
LUA_DIR="/etc/slurm/lua"

# --- obtain the seam files (from a local checkout, or a downloaded release) ---
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [[ -n "$SOURCE_DIR" ]]; then
  log "installing from local source: $SOURCE_DIR"
  SEAM="$SOURCE_DIR/seam"
  [[ -f "$SEAM/lua/job_submit.lua" ]] || die "no seam/ under $SOURCE_DIR"
  # Expect prebuilt linux binaries in $SOURCE_DIR/bin.
  OBOLD_BIN="$SOURCE_DIR/bin/obold"
  OBOL_BIN="$SOURCE_DIR/bin/obol"
  [[ -x "$OBOLD_BIN" && -x "$OBOL_BIN" ]] || die "build bin/obold and bin/obol first (make build)"
else
  arch="$(uname -m)"
  case "$arch" in
    x86_64)  gozarch="amd64" ;;
    aarch64) gozarch="arm64" ;;
    *) die "unsupported arch: $arch" ;;
  esac
  if [[ -z "$RELEASE" ]]; then
    log "resolving latest obol release"
    RELEASE="$(curl -fsSL https://api.github.com/repos/scttfrdmn/obol/releases/latest \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
    [[ -n "$RELEASE" ]] || die "could not resolve latest release (set --release)"
  fi
  log "downloading obol $RELEASE ($gozarch)"
  archive="obol_${RELEASE#v}_linux_${gozarch}.tar.gz"
  url="https://github.com/scttfrdmn/obol/releases/download/${RELEASE}/${archive}"
  curl -fsSL "$url" -o "$WORK/obol.tgz" || die "download failed: $url"
  tar -C "$WORK" -xzf "$WORK/obol.tgz"
  OBOLD_BIN="$WORK/obold"
  OBOL_BIN="$WORK/obol"
  # The release archive also carries the seam/ tree.
  SEAM="$WORK/seam"
  [[ -d "$SEAM" ]] || SEAM="$WORK"   # tolerate a flat archive
fi

# --- install binaries ---
log "installing binaries to $BINDIR"
install -d -m 0755 "$BINDIR"
install -m 0755 "$OBOLD_BIN" "$BINDIR/obold"
install -m 0755 "$OBOL_BIN"  "$BINDIR/obol"

# --- install the Lua seam ---
log "installing Lua seam to $LUA_DIR and $SLURM_ETC"
install -d -m 0755 "$LUA_DIR"
install -m 0644 "$SEAM/lua/obol_wire.lua"      "$LUA_DIR/obol_wire.lua"
install -m 0644 "$SEAM/lua/obol_transport.lua" "$LUA_DIR/obol_transport.lua"
install -m 0644 "$SEAM/lua/job_submit.lua"     "$SLURM_ETC/job_submit.lua"

# --- install prolog (BIND) and jobcomp (SETTLE) ---
# The BIND runs as PrologSlurmctld on the controller, in slurmctld's minimal
# environment. Wrap the shared prolog so OBOL_BIN/OBOL_SOCKET are always set
# (slurmctld does not inherit them, and /usr/local/bin may be off PATH).
log "installing prolog + jobcomp scripts"
install -m 0755 "$SEAM/slurm/obol-jobcomp.sh" "$BINDIR/obol-jobcomp.sh"
install -m 0755 "$SEAM/slurm/obol-prolog.sh"  "$BINDIR/obol-prolog.sh"
cat > "$BINDIR/obol-prolog-slurmctld.sh" <<EOF
#!/bin/bash
# Generated by install-obol.sh — PrologSlurmctld wrapper. Sets the obol env
# (slurmctld runs prologs with a minimal environment) then runs the shared BIND.
export OBOL_BIN="$BINDIR/obol"
export OBOL_SOCKET="$SOCKET"
exec "$BINDIR/obol-prolog.sh"
EOF
chmod 0755 "$BINDIR/obol-prolog-slurmctld.sh"

# --- obold config ---
install -d -m 0755 /etc/obol
if [[ -n "$CONFIG_SRC" ]]; then
  log "installing obold config from $CONFIG_SRC"
  install -m 0644 "$CONFIG_SRC" /etc/obol/obold.json
elif [[ ! -f /etc/obol/obold.json ]]; then
  log "writing starter obold config to /etc/obol/obold.json (EDIT ME)"
  cat > /etc/obol/obold.json <<'EOF'
{
  "admin_users": ["root"],
  "accounts": [
    {"name": "default", "balance": 100000, "rate": 1, "window": "720h"}
  ]
}
EOF
else
  log "keeping existing /etc/obol/obold.json"
fi

# --- systemd unit ---
log "installing obold.service (state-dir=$STATE_DIR socket=$SOCKET)"
install -d -m 0750 "$STATE_DIR"
cat > /etc/systemd/system/obold.service <<EOF
[Unit]
Description=obol budget daemon
After=network.target
Before=slurmctld.service

[Service]
ExecStart=$BINDIR/obold -socket $SOCKET -state-dir $STATE_DIR -config /etc/obol/obold.json
Restart=on-failure
RestartSec=2s
RuntimeDirectory=obol
User=root
Group=root

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now obold.service
log "obold started"

# Give obold a moment, then confirm reachability.
sleep 2
if "$BINDIR/obol" --socket "$SOCKET" ping >/dev/null 2>&1; then
  log "obold reachable on $SOCKET"
else
  log "WARNING: obold did not answer ping yet — check 'journalctl -u obold'"
fi

# --- load the seam into a running slurmctld ---
# The slurm.conf directives come from CustomSlurmSettings (cluster YAML); this
# reconfigure makes slurmctld pick up the now-present job_submit.lua + scripts.
if command -v scontrol >/dev/null 2>&1; then
  log "scontrol reconfigure"
  scontrol reconfigure || log "WARNING: scontrol reconfigure failed — check slurm.conf CustomSlurmSettings"
fi

log "done. Verify with: obol --socket $SOCKET list"
