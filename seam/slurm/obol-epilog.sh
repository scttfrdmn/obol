#!/bin/bash
# obol-epilog.sh — Slurm epilog that SETTLEs a job's budget escrow on exit.
#
# Installed as Epilog= in slurm.conf. Runs per job on the compute node after the
# job finishes. It maps Slurm's exit state to an obol settle kind and calls the
# obol CLI (which talks to obold over the socket). This is the simple settlement
# feed for the integration tiers; the production feed is the slurmdbd completion
# stream (issue #13).
#
# Environment provided by Slurm: SLURM_JOB_ID, SLURM_JOB_EXIT_CODE (or
# SLURM_JOB_DERIVED_EC), SLURM_JOB_ELAPSED / times. We also read the obol socket
# from OBOL_SOCKET (default matches obold).
#
# Settlement kind:
#   - job hit its time limit  -> timeout   (no refund)
#   - job was cancelled        -> cancel    (bill elapsed)
#   - node failure / requeue   -> infrafail (flag routes bill vs write-off)
#   - otherwise (clean exit)   -> complete  (refund unused tail)

set -u

OBOL="${OBOL_BIN:-obol}"
SOCKET="${OBOL_SOCKET:-/run/obol/obold.sock}"
JOBID="${SLURM_JOB_ID:-}"

if [[ -z "$JOBID" ]]; then
  # Not in a job context; nothing to settle.
  exit 0
fi

# Elapsed seconds: prefer scontrol, fall back to 0. The daemon clamps runtime to
# the funded walltime, so an over-estimate never overspends.
elapsed=0
if command -v scontrol >/dev/null 2>&1; then
  # RunTime is HH:MM:SS or D-HH:MM:SS; convert to seconds.
  rt=$(scontrol show job "$JOBID" 2>/dev/null | sed -n 's/.*RunTime=\([^ ]*\).*/\1/p' | head -1)
  if [[ -n "${rt:-}" ]]; then
    days=0; hms="$rt"
    if [[ "$rt" == *-* ]]; then days="${rt%%-*}"; hms="${rt#*-}"; fi
    IFS=: read -r h m s <<<"$hms"
    elapsed=$(( (10#${days:-0}*86400) + (10#${h:-0}*3600) + (10#${m:-0}*60) + 10#${s:-0} ))
  fi
fi

# Map exit state to a settle kind. SLURM_JOB_EXIT_CODE2 / state env vars vary by
# version; use the state string when available.
state="${SLURM_JOB_STATE:-}"
kind="complete"
case "$state" in
  TIMEOUT)               kind="timeout" ;;
  CANCELLED)             kind="cancel" ;;
  NODE_FAIL|PREEMPTED)   kind="infrafail" ;;
  *)                     kind="complete" ;;
esac

case "$kind" in
  complete)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind complete --runtime "$elapsed"
    ;;
  timeout)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind timeout
    ;;
  cancel)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind cancel --elapsed "$elapsed"
    ;;
  infrafail)
    "$OBOL" --socket "$SOCKET" settle --jobid "$JOBID" --kind infrafail --elapsed "$elapsed"
    ;;
esac

# Never fail the epilog on a settle error — a stuck escrow is swept by the
# janitor, but a nonzero epilog can drain the node.
exit 0
