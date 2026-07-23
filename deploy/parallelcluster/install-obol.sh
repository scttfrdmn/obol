#!/bin/bash
# install-obol.sh — install the obol seam on an AWS ParallelCluster head node.
#
# ORDERING (the reason this script has phases): ParallelCluster starts slurmctld
# during its own chef bootstrap, BEFORE the OnNodeConfigured custom action runs.
# When CustomSlurmSettings carries JobSubmitPlugins=lua, slurmctld fatals at
# startup if job_submit.lua is not already on disk ("Unable to stat
# .../job_submit.lua") — which fails the whole head-node bootstrap. So the seam
# FILES must be laid down at OnNodeStart (the only hook that runs before Slurm
# starts), and the obold DAEMON started at OnNodeConfigured (once shared storage
# is mounted). This script does both, selected by --phase:
#
#   --phase files    lay down binaries + Lua seam + prolog/jobcomp (OnNodeStart)
#   --phase daemon   write config + systemd unit, start obold, reconfigure (OnNodeConfigured)
#   --phase all      both, in order (default; for a by-hand SSH install on an
#                    already-running head node, or a custom-AMI build)
#
# Wire it into the cluster YAML as TWO actions pointing at this one script — see
# cluster.sample.yaml. It can also be run by hand as root (`sudo ./install-obol.sh`).
#
# The slurm.conf directives themselves (JobSubmitPlugins / PrologSlurmctld /
# JobCompType / JobCompLoc) come from the cluster YAML's CustomSlurmSettings,
# because ParallelCluster manages slurm.conf and overwrites a hand-edit on update.
#
# Why PrologSlurmctld and not Prolog: ParallelCluster deny-lists Prolog and
# Epilog in CustomSlurmSettings (it uses them for its own node lifecycle).
# PrologSlurmctld is not deny-listed and runs on the head node — the controller
# where obold lives — so the BIND step attaches there instead. See
# docs/feasibility-parallelcluster.md.
#
# Arguments (all optional; also settable via the matching env var):
#   --phase P         | OBOL_PHASE      files | daemon | all   (default all)
#   --release VERSION | OBOL_RELEASE    GitHub release tag (e.g. v0.14.0); default latest
#   --bindir DIR      | OBOL_BINDIR     where obold/obol land (default /usr/local/bin)
#   --state-dir DIR   | OBOL_STATE_DIR  obold state dir (default /var/lib/obol; point
#                                       at a shared FS mount to survive head-node
#                                       replacement — see the README)
#   --socket PATH     | OBOL_SOCKET     obold socket (default /run/obol/obold.sock)
#   --config PATH     | OBOL_CONFIG     obold.json to install (default: a starter config)
#   --source DIR      | OBOL_SOURCE     install from a local checkout (with bin/obold,
#                                       bin/obol, seam/) instead of a release archive
#
# Idempotent: re-running any phase is safe.

set -euo pipefail

log() { echo "[install-obol] $*"; }
die() { echo "[install-obol] ERROR: $*" >&2; exit 1; }

PHASE="${OBOL_PHASE:-all}"
RELEASE="${OBOL_RELEASE:-}"
BINDIR="${OBOL_BINDIR:-/usr/local/bin}"
STATE_DIR="${OBOL_STATE_DIR:-/var/lib/obol}"
SOCKET="${OBOL_SOCKET:-/run/obol/obold.sock}"
CONFIG_SRC="${OBOL_CONFIG:-}"
SOURCE_DIR="${OBOL_SOURCE:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --phase)     PHASE="$2"; shift 2 ;;
    --release)   RELEASE="$2"; shift 2 ;;
    --bindir)    BINDIR="$2"; shift 2 ;;
    --state-dir) STATE_DIR="$2"; shift 2 ;;
    --socket)    SOCKET="$2"; shift 2 ;;
    --config)    CONFIG_SRC="$2"; shift 2 ;;
    --source)    SOURCE_DIR="$2"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

case "$PHASE" in files|daemon|all) ;; *) die "invalid --phase: $PHASE (files|daemon|all)" ;; esac
[[ $EUID -eq 0 ]] || die "must run as root (use sudo)"

# The lua modules live in a fixed dir that matches job_submit.lua's OBOL_LUA_DIR
# default, so no per-node env is needed for the embedded slurmctld interpreter.
LUA_DIR="/etc/slurm/lua"

# job_submit/lua loads job_submit.lua from the slurm.conf directory. On
# ParallelCluster that's /opt/slurm/etc (Slurm is baked into the PC AMI there);
# a self-managed cluster typically uses /etc/slurm. Prefer whichever exists,
# preferring the PC location.
find_slurm_etc() {
  if [[ -n "${SLURM_CONF:-}" ]]; then dirname "$SLURM_CONF"; return; fi
  for d in /opt/slurm/etc /etc/slurm; do
    [[ -d "$d" ]] && { echo "$d"; return; }
  done
  # Nothing yet (very early OnNodeStart): default to the PC path and create it.
  echo /opt/slurm/etc
}

# ---------------------------------------------------------------------------
# phase: files — everything that must exist before slurmctld starts.
# ---------------------------------------------------------------------------
do_files() {
  local SLURM_ETC; SLURM_ETC="$(find_slurm_etc)"
  log "phase=files  slurm etc dir: $SLURM_ETC  lua dir: $LUA_DIR"

  WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' RETURN

  local OBOLD_BIN OBOL_BIN SEAM
  if [[ -n "$SOURCE_DIR" ]]; then
    log "installing from local source: $SOURCE_DIR"
    SEAM="$SOURCE_DIR/seam"
    [[ -f "$SEAM/lua/job_submit.lua" ]] || die "no seam/ under $SOURCE_DIR"
    OBOLD_BIN="$SOURCE_DIR/bin/obold"; OBOL_BIN="$SOURCE_DIR/bin/obol"
    [[ -x "$OBOLD_BIN" && -x "$OBOL_BIN" ]] || die "build bin/obold and bin/obol first (make build)"
  else
    local arch gozarch; arch="$(uname -m)"
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
    local archive url; archive="obol_${RELEASE#v}_linux_${gozarch}.tar.gz"
    url="https://github.com/scttfrdmn/obol/releases/download/${RELEASE}/${archive}"
    curl -fsSL "$url" -o "$WORK/obol.tgz" || die "download failed: $url"
    tar -C "$WORK" -xzf "$WORK/obol.tgz"
    OBOLD_BIN="$WORK/obold"; OBOL_BIN="$WORK/obol"
    SEAM="$WORK/seam"; [[ -d "$SEAM" ]] || SEAM="$WORK"   # tolerate a flat archive
  fi

  log "installing binaries to $BINDIR"
  install -d -m 0755 "$BINDIR"
  install -m 0755 "$OBOLD_BIN" "$BINDIR/obold"
  install -m 0755 "$OBOL_BIN"  "$BINDIR/obol"

  log "installing Lua seam to $LUA_DIR and $SLURM_ETC"
  install -d -m 0755 "$SLURM_ETC" "$LUA_DIR"
  install -m 0644 "$SEAM/lua/obol_wire.lua"      "$LUA_DIR/obol_wire.lua"
  install -m 0644 "$SEAM/lua/obol_transport.lua" "$LUA_DIR/obol_transport.lua"
  install -m 0644 "$SEAM/lua/job_submit.lua"     "$SLURM_ETC/job_submit.lua"

  log "installing prolog (BIND) + jobcomp (SETTLE) scripts"
  install -m 0755 "$SEAM/slurm/obol-jobcomp.sh" "$BINDIR/obol-jobcomp.sh"
  install -m 0755 "$SEAM/slurm/obol-prolog.sh"  "$BINDIR/obol-prolog.sh"
  # PrologSlurmctld wrapper: slurmctld runs prologs with a MINIMAL environment
  # (no OBOL_* vars, and scontrol is NOT on PATH). obol-prolog.sh needs scontrol
  # to read the correlation token out of the job's admin_comment — without it,
  # the BIND silently no-ops and the escrow is left unbound. So put the Slurm bin
  # dir on PATH here alongside OBOL_BIN/OBOL_SOCKET. (Learned on a live cluster.)
  local slurm_bin; slurm_bin="$(dirname "$SLURM_ETC")/bin"
  [[ -d "$slurm_bin" ]] || slurm_bin="/opt/slurm/bin"
  cat > "$BINDIR/obol-prolog-slurmctld.sh" <<EOF
#!/bin/bash
export OBOL_BIN="$BINDIR/obol"
export OBOL_SOCKET="$SOCKET"
export PATH="$slurm_bin:\$PATH"
exec "$BINDIR/obol-prolog.sh"
EOF
  chmod 0755 "$BINDIR/obol-prolog-slurmctld.sh"

  ensure_luasocket
  log "phase=files done — slurmctld can now load job_submit/lua"
}

# ensure_luasocket — the GATE shim (job_submit.lua) reaches obold via
# obol_transport.lua, which needs a Lua socket backend: luasocket's socket.unix,
# or a LuaJIT FFI. Stock ParallelCluster (alinux2023 / Slurm 25.11's embedded
# Lua 5.4) ships NEITHER, and there is no lua-socket package / EPEL / luarocks —
# so every gate fails "no usable socket backend". Build luasocket (incl. the
# AF_UNIX submodule) from source when absent, and place it where the embedded
# slurmctld interpreter's cpath will find it (/usr/lib64/lua/5.4). lua-devel +
# gcc are present on the PC AMI. No-op if a backend already loads.
ensure_luasocket() {
  local luav="5.4"
  if command -v lua >/dev/null 2>&1; then
    luav="$(lua -e 'print(string.match(_VERSION,"%d+%.%d+"))' 2>/dev/null || echo 5.4)"
  fi
  local target="/usr/lib64/lua/${luav}/socket/unix.so"
  if lua -e 'os.exit(pcall(require,"socket.unix") and 0 or 1)' 2>/dev/null; then
    log "luasocket socket.unix already available"; return
  fi
  if [[ -f "$target" ]]; then
    log "luasocket unix.so already installed at $target"; return
  fi
  if ! command -v gcc >/dev/null 2>&1 || [[ ! -e /usr/include/lua.h && ! -e "/usr/include/lua${luav}/lua.h" ]]; then
    log "WARNING: no luasocket and cannot build it (need gcc + lua-devel) — the"
    log "         GATE will fail closed. Install a Lua socket backend on this AMI."
    return
  fi
  log "building luasocket socket.unix from source (Lua $luav)"
  local ls_ver="3.1.0" d; d="$(mktemp -d)"
  local inc="/usr/include"; [[ -e "/usr/include/lua${luav}/lua.h" ]] && inc="/usr/include/lua${luav}"
  if curl -fsSL "https://github.com/lunarmodules/luasocket/archive/refs/tags/v${ls_ver}.tar.gz" -o "$d/ls.tgz" \
     && tar -C "$d" -xzf "$d/ls.tgz"; then
    ( cd "$d/luasocket-${ls_ver}/src"
      install -d "/usr/lib64/lua/${luav}/socket" "/usr/share/lua/${luav}"
      # core.so (socket base) + unix.so (AF_UNIX). unix.c needs the unixstream/
      # unixdgram sources or it fails with undefined unixstream_open.
      gcc -O2 -fPIC -I"$inc" -shared \
        luasocket.c timeout.c buffer.c io.c auxiliar.c options.c \
        inet.c usocket.c except.c select.c tcp.c udp.c \
        -o "/usr/lib64/lua/${luav}/socket/core.so" 2>/dev/null || true
      gcc -O2 -fPIC -I"$inc" -shared \
        unix.c unixstream.c unixdgram.c buffer.c io.c timeout.c usocket.c auxiliar.c options.c \
        -o "$target" 2>/dev/null || true
      install -m0644 socket.lua "/usr/share/lua/${luav}/socket.lua" 2>/dev/null || true
    )
  fi
  rm -rf "$d"
  if lua -e 'package.cpath="/usr/lib64/lua/'"${luav}"'/?.so;"..package.cpath; os.exit(pcall(require,"socket.unix") and 0 or 1)' 2>/dev/null; then
    log "luasocket socket.unix built OK at $target"
  else
    log "WARNING: luasocket build did not produce a loadable socket.unix — GATE may fail closed"
  fi
}

# ---------------------------------------------------------------------------
# phase: daemon — config + systemd unit + start obold + reconfigure slurmctld.
# Runs at OnNodeConfigured, once shared storage is mounted.
# ---------------------------------------------------------------------------
do_daemon() {
  log "phase=daemon  state-dir: $STATE_DIR  socket: $SOCKET"
  [[ -x "$BINDIR/obold" ]] || die "obold not installed — run phase=files first"

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

  # The GATE shim runs INSIDE slurmctld, which runs as the 'slurm' user on
  # ParallelCluster. obold's socket is created root-owned, and connect(2) on a
  # Unix socket needs WRITE permission — which 'slurm' lacks on a 0755 socket, so
  # every gate fails "budget daemon unreachable". obold's -socket-group /
  # -socket-mode flags (#136) set the socket to root:slurm 0660 at listen time so
  # slurmctld can connect while non-group users can't. On an older obold that
  # predates those flags, an ExecStartPost chgrp/chmod is the fallback.
  local sockgrp="slurm"
  getent group "$sockgrp" >/dev/null 2>&1 || sockgrp="root"
  local sockflags=""
  if "$BINDIR/obold" -help 2>&1 | grep -q -- "-socket-group"; then
    sockflags=" -socket-group $sockgrp -socket-mode 0660"
    log "obold supports -socket-group; socket will be $sockgrp:0660"
  fi

  log "installing obold.service (socket group: $sockgrp)"
  install -d -m 0750 "$STATE_DIR"
  {
    cat <<EOF
[Unit]
Description=obol budget daemon
After=network.target
Before=slurmctld.service

[Service]
ExecStart=$BINDIR/obold -socket $SOCKET -state-dir $STATE_DIR -config /etc/obol/obold.json${sockflags}
EOF
    # Fallback for an obold without the flags: fix perms after start.
    if [[ -z "$sockflags" ]]; then
      echo "ExecStartPost=/bin/sh -c 'for i in \$(seq 1 50); do [ -S \"$SOCKET\" ] && break; sleep 0.1; done; chgrp $sockgrp \"$SOCKET\" && chmod 0660 \"$SOCKET\"'"
    fi
    cat <<EOF
Restart=on-failure
RestartSec=2s
RuntimeDirectory=obol
User=root
Group=root

[Install]
WantedBy=multi-user.target
EOF
  } > /etc/systemd/system/obold.service

  systemctl daemon-reload
  systemctl enable --now obold.service
  sleep 2
  if "$BINDIR/obol" --socket "$SOCKET" ping >/dev/null 2>&1; then
    log "obold reachable on $SOCKET"
  else
    log "WARNING: obold did not answer ping yet — check 'journalctl -u obold'"
  fi

  # slurmctld is already running (started during bootstrap with job_submit.lua
  # present from phase=files); reconfigure so any config/script updates load.
  if command -v scontrol >/dev/null 2>&1 && scontrol ping >/dev/null 2>&1; then
    log "scontrol reconfigure"
    scontrol reconfigure || log "WARNING: scontrol reconfigure failed"
  fi
  log "phase=daemon done. Verify: $BINDIR/obol --socket $SOCKET list"
}

case "$PHASE" in
  files)  do_files ;;
  daemon) do_daemon ;;
  all)    do_files; do_daemon ;;
esac
