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

# Node-type cost true-up (issue #65): the prolog runs on the compute node AFTER
# Slurm has placed the job, so the node type is now known. Resolve it and pass it
# to bind so the daemon reprices the escrow from the worst-case estimate down to
# this node's actual rate. Resolution order:
#   1) OBOL_NODETYPE env (set per-node, e.g. via a NodeName->type table in the
#      node's environment or a gres/features mapping)
#   2) the node's Slurm "Features" (first feature), if scontrol is available
# Empty node type => bind without it => the worst-case escrow stands (safe).
node_type="${OBOL_NODETYPE:-}"
if [[ -z "$node_type" ]] && command -v scontrol >/dev/null 2>&1; then
  # Use the node Slurm assigned (SLURM_JOB_NODELIST is set in the prolog env),
  # not `hostname` — in a container hostname is the container id, not the Slurm
  # NodeName. Take the first node of the allocation and read its ActiveFeatures.
  node="${SLURM_JOB_NODELIST:-${SLURM_NODELIST:-}}"
  if [[ -n "$node" ]] && command -v scontrol >/dev/null 2>&1; then
    node=$(scontrol show hostnames "$node" 2>/dev/null | head -1)
    node_type=$(scontrol show node "$node" 2>/dev/null | sed -n 's/.*ActiveFeatures=\([^ ]*\).*/\1/p' | cut -d, -f1)
  fi
fi

# Array tasks (#103): the prolog env gives only SLURM_JOB_ID (the per-task id),
# NOT the array context — but `scontrol show job` on that id exposes ArrayTaskId
# (and the shared AdminComment token). When this job is an array task, bind its
# index so the daemon starts that task's slice of the array escrow. The token was
# stamped once for the whole array, so all tasks bind against the same token.
idx_arg=()
if command -v scontrol >/dev/null 2>&1; then
  atid=$(scontrol show job "$JOBID" 2>/dev/null | grep -oE 'ArrayTaskId=[0-9]+' | head -1 | cut -d= -f2)
  if [[ -n "$atid" ]]; then
    idx_arg=(--idx "$atid")
  fi
fi

if [[ -n "$node_type" ]]; then
  "$OBOL" --socket "$SOCKET" bind --token "$token" --jobid "$JOBID" --node-type "$node_type" "${idx_arg[@]}" || true
else
  "$OBOL" --socket "$SOCKET" bind --token "$token" --jobid "$JOBID" "${idx_arg[@]}" || true
fi
exit 0
