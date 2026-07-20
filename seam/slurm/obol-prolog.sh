#!/bin/bash
# obol-prolog.sh — Slurm prolog that BINDs a job's budget token to its job id.
#
# Installed as Prolog= in slurm.conf. Runs per job before it starts. It reads the
# correlation token the gate stamped into admin_comment and tells obold to bind
# token <-> jobid (which also fires the start event). After binding, the daemon's
# janitor can track the job by id, and the epilog can settle by job id.
#
# In production this binding is done by the site_factor plugin at pending->running
# (docs/SEAM_DESIGN.md §4); the prolog is the simpler equivalent for the
# integration tiers.

set -u

OBOL="${OBOL_BIN:-obol}"
SOCKET="${OBOL_SOCKET:-/run/obol/obold.sock}"
JOBID="${SLURM_JOB_ID:-}"

if [[ -z "$JOBID" ]]; then
  exit 0
fi

# Extract the budget token from admin_comment (format: "budget:<hex>", possibly
# among other space-separated tags).
token=""
if command -v scontrol >/dev/null 2>&1; then
  ac=$(scontrol show job "$JOBID" 2>/dev/null | sed -n 's/.*AdminComment=\([^ ]*\).*/\1/p' | head -1)
  for tok in $ac; do
    if [[ "$tok" == budget:* ]]; then token="$tok"; break; fi
  done
fi

if [[ -z "$token" ]]; then
  # No budget token: job was admitted fail-open (daemon was down at submit), or
  # this partition is ungated. Nothing to bind.
  exit 0
fi

"$OBOL" --socket "$SOCKET" bind --token "$token" --jobid "$JOBID" || true
exit 0
