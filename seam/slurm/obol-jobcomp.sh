#!/bin/bash
# obol-jobcomp.sh — controller-side completion feed for obol (issue #13).
#
# Installed as JobCompLoc under JobCompType=jobcomp/script. Slurm runs this on
# the CONTROLLER at every job completion, passing the job's final state and
# metrics in the environment. It maps the state to an obol settle kind and calls
# the obol CLI (which talks to obold over the socket).
#
# Why this and not just the epilog: the epilog runs on the compute node and can
# be skipped when a node fails — exactly the NODE_FAIL/infrafail case we must
# still bill/write-off correctly. jobcomp fires on the controller regardless, so
# it is the reliable settlement path; the epilog (if also installed) becomes a
# redundant fallback. A settle for an already-settled job is a harmless no-op
# (the daemon returns a clean rejection; we ignore it).
#
# jobcomp/script environment (Slurm): JOBID, JOBSTATE, ELAPSED (seconds or
# HH:MM:SS depending on version), plus many others. We read defensively.

set -u

# jobcomp/script runs from slurmctld with a MINIMAL environment: OBOL_BIN/
# OBOL_SOCKET are not inherited and /usr/local/bin is not on PATH. So default
# OBOL to an ABSOLUTE path (the overlay's install location) rather than a bare
# name, and fall back to a few common locations. The site can still override via
# OBOL_BIN/OBOL_SOCKET if it exports them into slurmctld's environment.
OBOL="${OBOL_BIN:-}"
if [[ -z "$OBOL" ]]; then
  for cand in /usr/local/bin/obol /usr/bin/obol; do
    [[ -x "$cand" ]] && OBOL="$cand" && break
  done
  OBOL="${OBOL:-/usr/local/bin/obol}"
fi
SOCKET="${OBOL_SOCKET:-/run/obol/obold.sock}"

JOBID="${JOBID:-${SLURM_JOB_ID:-}}"
STATE="${JOBSTATE:-${SLURM_JOB_STATE:-}}"

if [[ -z "$JOBID" ]]; then
  exit 0
fi

# ELAPSED may be seconds (integer) or HH:MM:SS / D-HH:MM:SS. Normalize to seconds.
elapsed_raw="${ELAPSED:-0}"
elapsed=0
if [[ "$elapsed_raw" =~ ^[0-9]+$ ]]; then
  elapsed="$elapsed_raw"
elif [[ -n "$elapsed_raw" ]]; then
  days=0; hms="$elapsed_raw"
  if [[ "$elapsed_raw" == *-* ]]; then days="${elapsed_raw%%-*}"; hms="${elapsed_raw#*-}"; fi
  IFS=: read -r h m s <<<"$hms"
  elapsed=$(( (10#${days:-0}*86400) + (10#${h:-0}*3600) + (10#${m:-0}*60) + 10#${s:-0} ))
fi

# Map final state to a settle kind. Slurm state strings can carry a suffix
# (e.g. "CANCELLED by 1000"), so match the leading word.
case "$STATE" in
  TIMEOUT*)              kind="timeout" ;;
  CANCELLED*)            kind="cancel" ;;
  NODE_FAIL*|PREEMPTED*|BOOT_FAIL*) kind="infrafail" ;;
  *)                     kind="complete" ;;
esac

case "$kind" in
  complete)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind complete --runtime "$elapsed" --if-present ;;
  timeout)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind timeout --if-present ;;
  cancel)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind cancel --elapsed "$elapsed" --if-present ;;
  infrafail)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind infrafail --elapsed "$elapsed" --if-present ;;
esac

# Never fail the completion hook on a settle error (already-settled, or daemon
# hiccup — the janitor is the backstop). A nonzero here must not disrupt Slurm.
exit 0
