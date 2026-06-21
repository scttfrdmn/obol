#!/usr/bin/env bash
#
# bootstrap-github.sh — one-time project setup. Creates labels, milestones, and
# seeds the build plan as issues. Planning lives in GitHub, not in local files;
# this script is the only place the initial plan is written down, and after it
# runs the GitHub project IS the plan.
#
# Requires: gh (authenticated), run from the repo root with the remote set.
# Idempotent-ish: re-running skips labels/milestones that already exist.
#
# Usage:
#   ./scripts/bootstrap-github.sh            # create everything
#   ./scripts/bootstrap-github.sh --labels   # labels only
#   ./scripts/bootstrap-github.sh --dry-run  # print what it would do

set -euo pipefail

DRY=0
ONLY=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --labels) ONLY="labels" ;;
    --milestones) ONLY="milestones" ;;
    --issues) ONLY="issues" ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

run() { if [ "$DRY" = 1 ]; then echo "+ $*"; else "$@"; fi; }

REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
echo "Repo: $REPO"

# ---------------------------------------------------------------------------
# Labels (parsed from .github/labels.yml — kept simple: name/color/description)
# ---------------------------------------------------------------------------
create_labels() {
  echo "== Labels =="
  # crude YAML reader for our fixed shape: - { name: "x", color: "y", description: "z" }
  grep -E '^\- \{' .github/labels.yml | while IFS= read -r line; do
    name=$(sed -E 's/.*name: *"([^"]*)".*/\1/' <<<"$line")
    color=$(sed -E 's/.*color: *"([^"]*)".*/\1/' <<<"$line")
    desc=$(sed -E 's/.*description: *"([^"]*)".*/\1/' <<<"$line")
    run gh label create "$name" --color "$color" --description "$desc" --force
  done
}

# ---------------------------------------------------------------------------
# Milestones (via REST; gh has no native milestone create)
# ---------------------------------------------------------------------------
milestone_number() {
  gh api "repos/$REPO/milestones?state=all" --jq ".[] | select(.title==\"$1\") | .number" 2>/dev/null || true
}
create_milestone() {
  local title="$1" desc="$2"
  if [ -n "$(milestone_number "$title")" ]; then echo "  = exists: $title"; return; fi
  run gh api "repos/$REPO/milestones" -f title="$title" -f description="$desc" >/dev/null
  echo "  + $title"
}
create_milestones() {
  echo "== Milestones =="
  create_milestone "v0.1.0 — obold MVP" \
    "Go sidecar daemon wrapping the proven kernel: wire protocol, in-memory gate, group-commit durability, tier-2 read path. Testable without Slurm."
  create_milestone "v0.2.0 — Gen 1 integration" \
    "Lua job_submit shim + burstlab Gen 1 (Slurm 22.05) validation. Money gate end-to-end on a real cluster."
  create_milestone "v0.3.0 — Burst dispatch + multi-gen" \
    "site_factor priority plugin realizing burst-as-dispatch-gate; Gen 2/3 (23.11/24.05) validation."
  create_milestone "v0.4.0 — Hierarchy & resolution" \
    "Re-expand the model: account-tree hierarchy/rollup, multi-budget resolve/disambiguation, partitions."
  create_milestone "CLI / budget management" \
    "The admin-facing budget management surface (your original 'commands to manage budgets comprehensively'). A feature epic spanning releases: single-budget verbs are buildable now; attachment/resolution/transfer verbs gate on the v0.4.0 model work."
}

# ---------------------------------------------------------------------------
# Issues — the build plan. Each maps to a milestone and labels.
# Derived from docs/SEAM_DESIGN.md §13 (known gaps) and the component plan.
# ---------------------------------------------------------------------------
mk_issue() {
  local title="$1" ms="$2" labels="$3" body="$4"
  run gh issue create --title "$title" --milestone "$ms" --label "$labels" --body "$body"
}
create_issues() {
  echo "== Issues =="

  # --- v0.1.0 obold MVP ---
  mk_issue "Define the shim↔daemon wire protocol (GATE/BIND/SETTLE)" \
    "v0.1.0 — obold MVP" "type:feat,area:protocol,priority:p0" \
    "Implement the length-prefixed local-socket protocol from SEAM_DESIGN.md §8. GATE returns once the in-memory escrow is made; durability completes async. Acceptance: encode/decode round-trip tests; protocol versioned."

  mk_issue "obold: Unix-socket server wrapping the kernel" \
    "v0.1.0 — obold MVP" "type:feat,area:daemon,priority:p0" \
    "Daemon serving GATE/BIND/SETTLE against internal/budget. Acceptance: end-to-end test driving the kernel through the socket; clean shutdown; conservation holds across a simulated session."

  mk_issue "Group commit for the WAL (off the controller lock)" \
    "v0.1.0 — obold MVP" "type:perf,area:wal,priority:p0" \
    "SEAM_DESIGN.md §3/§13.3. Batch appends, one fdatasync, release waiters. The GATE ack returns after the in-memory escrow; durability lands a hair later. Acceptance: throughput test; crash test still recovers; torn-tail discipline preserved."

  mk_issue "Tier-2 read path: lock-cheap snapshot of (balance, burstPot, rLive)" \
    "v0.1.0 — obold MVP" "type:perf,area:daemon,priority:p1" \
    "SEAM_DESIGN.md §3/§13.6. Priority reads run O(pending) per cycle and must not contend the gate write lock. Copy-on-write or atomic snapshot. Acceptance: race test with concurrent gate writes + heavy reads; no contention regression."

  mk_issue "Decide & implement config durability (flags as logged commands vs immutable)" \
    "v0.1.0 — obold MVP" "type:feat,area:kernel,status:needs-design,priority:p1" \
    "SEAM_DESIGN.md §13.4. Policy flags currently persist only via snapshot. Either make config changes logged commands, or make config immutable post-creation. Open the design discussion in this issue before coding."

  mk_issue "Simulate shim fail-open/closed + timeout in Go tests" \
    "v0.1.0 — obold MVP" "type:test,area:protocol,priority:p1" \
    "Per-partition fail mode (SEAM_DESIGN.md §6/§7). Daemon-down and daemon-slow cases; cloud fails closed, on-prem fails open; hard timeout treats slow as down. Acceptance: tests covering both partition classes and the timeout boundary."

  # --- v0.2.0 Gen 1 integration ---
  mk_issue "Confirm admin_comment read+write in Lua job_desc on Slurm 22.05" \
    "v0.2.0 — Gen 1 integration" "type:test,area:lua-shim,slurm:gen1-2205,priority:p0,status:blocked" \
    "SEAM_DESIGN.md §13.1 — the Gen 1 blocker. Grep _get_job_req_field/_set_job_req_field in 22.05's job_submit_lua.c and verify on a burstlab Gen 1 cluster that the shim can stamp a token into admin_comment and that it survives to the completion record."

  mk_issue "Write job_submit.lua shim (Gen 1): resolve, gate call, token stamp" \
    "v0.2.0 — Gen 1 integration" "type:feat,area:lua-shim,slurm:gen1-2205,priority:p0" \
    "Reads account/partition/uid/time_limit/tres_* from job_desc, calls daemon GATE, sets admin_comment, returns SUCCESS or rejects with err_msg. Must never touch disk (controller lock). Validate on burstlab Gen 1."

  mk_issue "Cost model: derive c from TRESBillingWeights / cons_tres" \
    "v0.2.0 — Gen 1 integration" "type:feat,area:daemon,priority:p1" \
    "SEAM_DESIGN.md §5. Ride Slurm's existing TRES billing rather than a parallel rate table; GPU-weighted via tres_per_*. Worst-case escrow + dispatch true-up for cost-heterogeneous partitions."

  mk_issue "Completion/infra-fail event feed from slurmdbd → SETTLE" \
    "v0.2.0 — Gen 1 integration" "type:feat,area:daemon,priority:p1" \
    "Subscribe to the accounting/completion path (jobcomp vs slurmdbd record per version) and drive Complete/Timeout/InfraFail. Determine reliability → sets janitor sweep cadence."

  # --- v0.3.0 burst dispatch + multi-gen ---
  mk_issue "site_factor plugin: burst dispatch gate + token↔jobid bind + start event" \
    "v0.3.0 — Burst dispatch + multi-gen" "type:feat,area:site-factor,priority:p0" \
    "SEAM_DESIGN.md §4. One plugin, triple duty under priority/multifactor. Returns priority 0 when no burst headroom; binds token↔jobid; triggers burst reservation on pending→running."

  mk_issue "Unbound-token TTL in the janitor (submit→start orphan window)" \
    "v0.3.0 — Burst dispatch + multi-gen" "type:feat,area:wal,priority:p1" \
    "SEAM_DESIGN.md §4/§13.2. An escrow against a token never bound to a jobid (daemon crashed in the submit→start gap) can't be swept by jobid. Add a TTL sweep for unbound tokens older than N cycles."

  mk_issue "Validate on burstlab Gen 2 (23.11) and Gen 3 (24.05)" \
    "v0.3.0 — Burst dispatch + multi-gen" "type:test,area:lua-shim,slurm:gen2-2311,slurm:gen3-2405,priority:p1" \
    "Run the per-generation verification checklist (SEAM_DESIGN.md §10) against Gen 2 and Gen 3 clusters; isolate any ABI deltas to the shim."

  # --- v0.4.0 hierarchy ---
  mk_issue "Account-tree hierarchy & rollup (child spend debits ancestors)" \
    "v0.4.0 — Hierarchy & resolution" "type:feat,area:kernel,status:needs-design,priority:p1" \
    "Re-expand the model deferred to reach the single-budget core. Chain-debit under one lock; conservation across the chain; sibling-vs-ancestor disambiguation rule."

  mk_issue "Multi-budget resolution & disambiguation at job_submit" \
    "v0.4.0 — Hierarchy & resolution" "type:feat,area:daemon,status:needs-design,priority:p1" \
    "SEAM_DESIGN.md §9. account+partition → budget; one resolves→use, many→reject asking to specify, none→reject. Auto-resolve to most specific account, roll up."

  # --- CLI / budget management (the original 'manage budgets comprehensively') ---
  mk_issue "DECISION: which obol management ops go via the socket vs daemon-direct" \
    "CLI / budget management" "type:feat,area:cli,area:protocol,status:needs-design,priority:p0" \
    "The binary split is settled by naming: obol (CLI) + obold (daemon), the standard command/daemon pair. The remaining open question this issue tracks: does obol route ALL management ops through obold over the Unix socket (single authority, daemon must be up), or may some read-only/offline ops (e.g. inspecting a snapshot, audit-log render) read state directly without the daemon? Decide here; the verbs below build against the answer."

  mk_issue "CLI: obol scaffold + single-budget lifecycle verbs (create, show, set-window, set-rate)" \
    "CLI / budget management" "type:feat,area:cli,priority:p1" \
    "Buildable now against the kernel (single-budget core). 'obol create' a budget; 'obol show' balance, current burn rate, time-to-empty; 'obol set-window' / 'obol set-rate'. Depends on the DECISION issue (socket vs direct). Acceptance: verbs drive the kernel/daemon; show output matches BurstSnapshot + conservation."

  mk_issue "CLI: obol simulate / estimate (will this job fund? projected runway)" \
    "CLI / budget management" "type:feat,area:cli,priority:p2" \
    "Read-only 'would this fit' against a budget without committing (small kernel addition: check cost<=B and burst headroom without debit). Acceptance: simulate a hypothetical job, report ALLOW/DENY + projected time-to-empty. Buildable now for a single budget."

  mk_issue "CLI: obol log — transaction / audit view (render the WAL)" \
    "CLI / budget management" "type:feat,area:cli,area:wal,priority:p1" \
    "The WAL is already an append-only audit trail of every transition. Surface a 'log' verb that renders it human-readably (per-budget, per-job, time-ordered). Buildable now. Acceptance: log reflects submit/start/settle/lapse events with amounts."

  mk_issue "CLI: obol attach / detach user|group|partition to a budget" \
    "CLI / budget management" "type:feat,area:cli,status:blocked,priority:p1" \
    "BLOCKED on v0.4.0 hierarchy/resolution: 'attach users/groups to budgets' and 'budgets to partitions' need the multi-budget model that the kernel doesn't yet implement. Track here; unblock when account-tree hierarchy lands."

  mk_issue "CLI: obol resolve (given user+partition, show matching budget(s) and why)" \
    "CLI / budget management" "type:feat,area:cli,status:blocked,priority:p1" \
    "BLOCKED on multi-budget resolution (v0.4.0). The disambiguation UX: show which budget(s) a user+partition resolves to and the resolution reasoning. Essential once more than one budget can resolve."

  mk_issue "CLI: obol transfer / reassign leftover at period boundary" \
    "CLI / budget management" "type:feat,area:cli,status:blocked,priority:p2" \
    "BLOCKED on hierarchy. The between-windows admin function: move a lapsed budget's remaining balance to a parent or sibling, or re-enable with a fresh window. Conservation must hold across the transfer."
}

case "$ONLY" in
  labels) create_labels ;;
  milestones) create_milestones ;;
  issues) create_issues ;;
  *) create_labels; create_milestones; create_issues ;;
esac

echo "Done. The GitHub project is now the plan — keep it there, not in local files."
