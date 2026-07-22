# Slurm Budget Plugin — Seam Design

How the proven budget kernel attaches to a running Slurm cluster.

## Status

Two things exist at different maturities:

- **The kernel** (this repo, Go): money ledger with an atomic submit-gate, escrow/refund,
  per-partition policy flags, lapse, job arrays, and an explicit burst token bucket — all
  holding conservation and non-negativity under the race detector, with crash-safe durability
  (command-logged WAL + snapshot recovery + orphan janitor). This is **built and tested**.
- **The seam** (this document): how that kernel meets Slurm's plugin surface. The **daemon
  (`obold`), the wire protocol, the GATE Lua shim, and the prolog/jobcomp/epilog scripts are
  built and tested** (the last in a single-node Slurm Docker tier). The **burst dispatch
  decision** is built and tested too — in the daemon (`obol dispatch` / `handleDispatch` →
  `Budget.MayDispatch`); the `site_factor` C plugin that calls it ships as **reference source**
  (`seam/plugin/`), compiled per site against its own Slurm tree and validated on a real
  cluster (burstlab), since the hook has no Lua binding and can't run in the RPM-based tier.

The seam is targeted at three Slurm minor versions, matching burstlab's generations:
**22.05, 23.11, 24.05** (Rocky 8/9/10 and Ubuntu 22.04/24.04). Where their plugin ABIs
differ, the design isolates the difference to the smallest possible per-generation surface.

---

## 1. Why this shape: the controller-lock constraint

The single hard fact that determines the entire architecture:

> Slurm runs the `job_submit` plugin **inside `slurmctld` while holding internal locks**, and
> only one copy runs at a time. This serializes most controller work behind the plugin. SchedMD
> explicitly advises against doing slow work here, and recommends against a Lua `job_submit`
> plugin in high-throughput environments for this reason.

Consequences, all forced rather than chosen:

1. **The submit hook must not block on I/O.** No `fsync`, no slow IPC, no disk. Anything that
   could stall stalls the whole controller for every user.
2. **Durability cannot live in the hook.** The WAL append must happen *off* the controller lock.
3. **The authoritative state lives in a sidecar daemon, not in-process.** A `.so` doing I/O
   inside `slurmctld` is a site-down risk, and would need ABI-matched rebuilds against three
   Slurm minors across two OS families. A thin Lua shim talking to a Go daemon over a local
   socket has one tiny per-generation surface and a kernel that does not care what version calls it.

This is the answer to "sidecar vs in-process": **sidecar**, forced by the lock constraint, not
chosen on taste.

---

## 2. Components

```
        sbatch / salloc / srun / slurmrestd
                      │
                      ▼  (job submission)
        ┌─────────────────────────────┐
        │ slurmctld                   │
        │                             │
        │  job_submit.lua  ───────────┼──── Unix socket ────►  ┌──────────────────┐
        │   (gate, on lock, FAST)     │      (gate call)       │ obold (Go)     │
        │                             │                        │                  │
        │  site_factor plugin ────────┼──── Unix socket ────►  │  proven kernel   │
        │   (burst gate + bind +      │   (bind / burst /      │  WAL + snapshot  │
        │    start event, per cycle)  │    start event)        │  orphan janitor  │
        │                             │                        │                  │
        └─────────────┬───────────────┘                        └────────┬─────────┘
                      │ completion / node-fail                          │ durability
                      ▼  (off the lock)                                 ▼  (off the lock)
            slurmdbd accounting  ───── Unix socket ──► obold          disk (WAL, snapshot)
```

- **`job_submit.lua`** — tiny, per-generation, version-portable. Reads the submission from
  `job_desc`, makes one fast loopback call to `obold`, returns SUCCESS or rejects with a
  message. Stamps a correlation token into `admin_comment`. **Never touches disk.**
- **`obold`** — Go daemon holding the proven kernel. Owns the WAL, snapshot, and janitor.
  Serves a sub-millisecond in-memory gate. Does durability off the controller lock.
- **`site_factor` plugin** — runs under `PriorityType=priority/multifactor`, called every
  scheduling cycle with the full job record. Realizes burst-as-dispatch-gate as a custom
  priority factor, and doubles as the token↔jobid binder and start-event source (see §4).
- **slurmdbd** — already mandatory in burstlab's config (Plugin v2 requires it). Provides the
  **account hierarchy** (our "groups") and the **completion / node-fail event feed**.

---

## 3. Three-tier latency model

The seam has three call paths with very different latency budgets. Conflating them was the
original mistake; separating them is what makes the hot path tractable.

| Tier | Call site | Frequency | On controller lock? | Budget | Durability |
|------|-----------|-----------|---------------------|--------|------------|
| **1. Submit gate** | `job_submit.lua` | every `sbatch` | **yes** | hardest (sub-ms) | append off-lock in daemon |
| **2. Priority/burst read** | `site_factor` | every cycle × every pending job | no (but frequent) | sub-µs, lock-cheap | none (pure read) |
| **3. Completion / node-fail** | slurmdbd feed | as jobs end | no | loosest | append, can batch |

Design implications:

- **Tier 1** is the only place the controller blocks on us. The gate read is in-memory in the
  daemon; the WAL append happens *after* the in-memory escrow, acknowledged to the shim once the
  reservation is made, with the disk write completing a hair later under **group commit**
  (batch several appends, one `fdatasync`, release waiters). The shim blocks on a memory op and
  a loopback round-trip, never on a disk.
- **Tier 2** is a *read-concurrency* problem, not a durability one. It runs O(pending jobs) per
  cycle, so it must not contend the gate's write lock. The daemon serves these reads from a
  copy-on-write / atomic snapshot of `(balance, burstPot, rLive)` separate from the write path.
- **Tier 3** has slack. Refunds are spread over wall-clock and naturally uncontended — the
  asymmetry we already exploit for burst (batch the contended debit, spread the cheap refunds).

---

## 4. Identity & correlation: the token lifecycle

**The problem.** At `job_submit` time the job has no job ID yet — Slurm assigns it only after the
plugin returns SUCCESS. But the gate must *escrow at submit* (escrow is the atomic check-and-debit;
checking without debiting reintroduces the overdraft race). So the escrow cannot be keyed by job ID.

**The mechanism.** A correlation **token** minted by the daemon at submit, carried through the
job's life in `admin_comment`:

- `admin_comment` is writable from `slurm_job_submit` (the NVIDIA production job-submit framework
  does exactly this), and being *admin*-controlled it cannot be tampered with by the user — so the
  token cannot be spoofed to dodge the gate. (`comment`, by contrast, is user-visible/editable.)

**The three-point lifecycle:**

```
SUBMIT  (job_submit.lua — tier 1, on the lock, fast)
  read account, partition, user_id, time_limit, tres_* from job_desc
  → obold: "escrow this cost?"   (in-memory escrow; WAL append off-lock)
  obold mints token T, escrows money against T, returns T
  shim sets job_desc.admin_comment += "budget:T"
  return SUCCESS, or set err_msg and reject

START   (site_factor — tier 2, every cycle, sees full job record)
  reads job state + admin_comment(T) + jobid
  → obold: bind T ↔ jobid     (now the janitor can check liveness by jobid)
  burst dispatch gate: return priority 0 when no burst headroom, else allow
  on pending→running: obold reserves burst, rLive += C

COMPLETE  (slurmdbd accounting feed — tier 3, off the lock)
  completion record carries jobid (and admin_comment T)
  → obold: settle T  (refund unused tail)

NODE_FAIL / PREEMPT  (controller event feed)
  → obold: InfraFail T  (bill-vs-writeoff per the partition's flag)

JANITOR  (periodic)
  for each bound, unsettled T: is jobid still alive in Slurm? if not, sweep
```

**`site_factor` does triple duty**, which is the elegant part: it is the burst dispatch gate
*and* the token↔jobid binder *and* the start-event source — because it is called every cycle with
the full job record (jobid, state, `admin_comment` all visible). One plugin, three needs, and it
is the same plugin that must exist anyway to realize burst as a priority factor.

**Job arrays (#103).** `slurm_job_submit` fires ONCE for a whole array; the shim reads the array
spec (`job_desc.array_inx`, e.g. `"0-9"` / `"0-9%4"` / `"1,3,5"`), counts the tasks, and gates all
N as a single array escrow (one token, stamped once in `admin_comment` — every task inherits it).
Bind and settle are then PER TASK: Slurm assigns each task its own job id plus an `ArrayTaskId`.
The prolog reads `ArrayTaskId` from `scontrol show job` (the array env vars are not exported to the
prolog, but the record carries them) and binds `(token, idx)`; jobcomp fires per task with
`ARRAYTASKID` in its environment and settles `(token, idx)`. Each task draws and refunds its own
`c*w` slice via the kernel's per-task transitions; the daemon drops the token routing when the last
task settles. **Caveat:** for a non-array job Slurm sets `ARRAYTASKID` to the `NO_VAL` sentinel
(`4294967294`), not unset — the scripts accept only a real small index, so a 1:1 job is never
mistaken for array task 4-billion.

**Known gap — the submit→start window.** Between `job_submit` (escrow against T) and `site_factor`
first seeing the job (T↔jobid bound), the daemon knows the job only as an unbound token. If the
daemon crashes and recovers in that window, it holds an escrow against T with no jobid, which the
jobid-based janitor cannot sweep. **Mitigation:** a TTL on unbound tokens — an unbound token older
than a few scheduling cycles is presumed dead and swept. This is a small addition to the janitor
and is the one orphan class the current sweep does not cover.

---

## 5. Cost model

Cost rides Slurm's existing machinery rather than running a parallel one:

- burstlab's Gen 1 already runs `SelectType=select/cons_tres` with `CR_Core`, so Slurm already
  tracks per-job consumable resources and can compute a billable TRES rate via `TRESBillingWeights`.
- The shim reads `tres_per_node`, `tres_per_task`, `tres_per_socket`, `tres_per_job` (the GPU-aware
  set — `--gres=gpu`→`tres_per_node`, `--gpus`→`tres_per_job`, etc.), plus `time_limit`, directly
  from `job_desc`. No extra lookup on the hot path.
- The kernel's `cost = c · w` is then: `c` = the partition's per-(core|GPU|TRES)-second rate,
  `w` = the requested time limit. GPU-weighted cost falls out naturally, which matters for
  GPU-heavy workloads.

**The placement caveat (unchanged from the kernel design):** at submit, node placement is unknown,
so for cost-heterogeneous partitions the shim escrows worst-case and the daemon trues up when the
job dispatches (tier 2 sees the real allocation). Cost-homogeneous partitions make this exact —
an argument for encouraging homogeneous-cost partitions.

**Implemented (issue #12):** the shim reads the requested TRES from `job_desc` and sends it on the
GATE request; the daemon maps it to a per-second rate via configured weights
(`-tres-per-cpu|gpu|mem`, `daemon.Weights.Rate`) and passes that rate into the kernel's
`SubmitAt`/`SubmitArrayAt`, which freeze it per-escrow and log it in the WAL. All-zero weights =
flat rate (the budget's `C`), the default.

**Node-type cost + true-up (issue #65, implemented).** Because a partition can contain multiple
node types, the real cost is set by the node Slurm *binds* — unknown at submit. So:
- Config carries `node_types` (name → rate, per `s`/`m`/`h`, normalized to integer units/second)
  and, per partition, the node types it can place on.
- **Submit (gate):** the node isn't chosen, so obol escrows the partition's **worst-case** (max)
  node rate × walltime — never under-escrows, preserving the no-overdraft invariant.
- **Dispatch (BIND):** the prolog runs post-placement, resolves the bound node's type (from
  `SLURM_JOB_NODELIST` → `ActiveFeatures`, or `OBOL_NODETYPE`), and passes it on BIND. The daemon
  calls the kernel's `Reprice` — which may only **lower** the rate (worst ≥ actual) — before
  `Start`, adjusting the escrow down to the real node's rate. Settlement then bills the trued rate.
- **Trade-off:** worst-case escrow can reject a job that would have been cheap on a tight budget —
  the correct conservative choice, and an argument for cost-homogeneous partitions.

---

## 6. Partition policy: one axis, three booleans

Three per-partition flags all track the same underlying property — **is this partition owned
hardware or rented cloud capacity?**

| partition class | `bill_infra_failures` | `allow_requeue` | `fail_closed` |
|-----------------|----------------------|-----------------|---------------|
| **cloud** (rented, real \$) | true | false | true |
| **on-prem** (owned, free) | false | true | false |

- `bill_infra_failures` — cloud: a preempted Spot node cost real money, bill it. On-prem: infra
  failure is free, write it off.
- `allow_requeue` — on-prem: re-running is cheap, allow it. Cloud: re-running costs again, treat
  requeue as cancel.
- `fail_closed` — cloud: never admit an unfunded job (the daemon being down must not bypass the
  gate). On-prem: never block users for a daemon hiccup (idle owned capacity is free).

They are exposed as three independent booleans (so an odd partition can break the pattern), but an
admin should be able to set a single `class: cloud | on-prem` that defaults all three, overriding
individually as needed.

**Placement of `fail_closed` is special:** it lives in the **shim's local config**, not the
daemon's state — because it is consulted exactly when the daemon is unreachable. The shim must
answer "open or closed for this partition?" from a static local table with no round-trip. The
daemon being down is the entire scenario.

---

## 7. Fail-mode behavior

When the daemon is slow or down, the shim decides per-partition via the local `fail_closed` table,
with these mechanics:

- **Hard timeout (~tens of ms).** "Slow" and "down" are the same to the controller lock — a hung
  daemon must not stall `slurmctld`. A healthy daemon answers in microseconds; the timeout only
  ever fires on genuine trouble.
- **Aggressive supervision.** `obold` runs under systemd with restart-always and a tight restart
  window, so "down" is measured in seconds.
- **Break-glass.** An admin can comment out the plugin line and `scontrol reconfigure` to restore
  submissions in seconds — the documented escape hatch when fail-closed cloud partitions are
  blocking submits during an incident.

Net behavior: a cloud partition fails closed (protecting real money) but recovers fast and has an
escape hatch; an on-prem partition fails open (never blocks users) and reconciles when the daemon
returns.

---

## 8. Wire protocol (sketch)

Local Unix-domain socket, length-prefixed messages. Three request types; minimal fields on the
hot path.

```
GATE   (tier 1, hot)
  req:  { account, partition, uid, time_limit, tres{...}, ntasks_or_array }
  resp: { allow: bool, token: T, reason?: string }
        // on allow, the daemon has already escrowed in-memory against T

BIND   (tier 2)
  req:  { token: T, jobid }
  resp: { ok: bool }
        // also the carrier for the start event / burst reservation trigger

SETTLE (tier 3)
  req:  { token: T | jobid, kind: complete|timeout|cancel|infrafail, runtime, now }
  resp: { ok: bool }
```

Notes:
- The GATE response is the only one on the controller lock; it returns as soon as the in-memory
  escrow is made. Durability completes asynchronously (group commit), which is safe because a
  crash before the append means the gate's SUCCESS was never durably acknowledged — consistent
  with the WAL's torn-tail discipline.
- `now` is carried explicitly on SETTLE (and implicitly stamped on GATE/BIND) because the kernel's
  transitions are pure functions of `(state, command, now)` — that is what makes the WAL replay
  deterministic. The daemon supplies `now` from its own clock at the moment of the call.

---

## 9. Resolution: account + partition → budget

- **"Group" = Slurm account.** Using slurmdbd's account tree (already running, already required)
  gives hierarchy, membership, and the `--account` resolution path for free, instead of
  maintaining a parallel identity store.
- At submit the shim has `account` + `partition`. The daemon resolves these to a budget:
  - exactly one budget resolves → use it;
  - more than one resolves → reject asking the user to specify (`--comment` / a budget field);
  - none resolves → reject (no funded path), even if the job would otherwise schedule.
**Implemented (issue #18) — flat per-account model.** obold holds a registry of independent
per-account budgets (`obold -config`), one WAL+snapshot dir each; the kernel is unchanged and each
account conserves on its own. Resolution is an **exact account-name match** — a sub-account with no
entry does not roll up, it fails to resolve → reject. There is **no account-tree chain-debit**; the
originally-planned rollup (#17) was superseded by this simpler model (spend spanning multiple
budgets is a separate future feature, #54). Access defaults to **trusting Slurm** (membership is
already enforced by slurmdbd); an account may optionally set an `allow_users`/`allow_groups`
list, in which case obol additionally resolves the submitter's uid→user/groups and checks it
(only then is the lookup incurred, keeping the hot path free of it by default).

### 9.1 Multi-source funding (issue #54)

A job may name an **ordered** list of source accounts (`GateRequest.Sources`) instead of a single
`--account`; empty falls back to single-source, byte-identical to before. The gate splits the job
by **ordered fallback**: source 1 funds the first `W₁` seconds of the job at the full rate `c`,
source 2 the next `W₂`, … with `Σ Wᵢ = W` (drain the lab grant, then spill to the startup
allocation). Each source funds only the **whole seconds** it can afford — `Wᵢ = floor(Bᵢ/c)`
capped by the remaining need — so each leg is an *ordinary single-budget escrow* with
`Reserved = c·Wᵢ`. **The kernel is unchanged**: each leg conserves on its own; there is **no
cross-budget conservation construct**. A source with `Bᵢ < c` funds zero whole seconds and its
sub-`c` remainder stays in its balance (not stranded). Fundable ⇔ `Σ floor(Bᵢ/c) ≥ W`.

The daemon composes the legs. `handleGate` places one escrow per funded source **sequentially,
one kernel lock at a time** (the transfer discipline — never two budget locks together); it is
**all-or-nothing** — if any leg fails to escrow, every already-placed leg is rolled back with a
full refund (`Cancel(legToken, 0)`) and the gate rejects, so no job is left partially funded. A
master token maps to the ordered legs in memory (`tokenLegs`, exactly as `tokenBudget` for
single-source). `handleBind` reprices (all-or-none, so per-leg rates can't diverge) and starts
every leg; `handleSettle` fans the terminal transition out to each leg with its apportioned
runtime slice `rᵢ = clamp(runtime − prefixᵢ, 0, Wᵢ)`, so `Σ billed = c·runtime` and an early
completion refunds the tail sources while billing the head ones. `InfraFail` applies each budget's
own `BillInfraFailures` policy to its slice, so mixed cloud/on-prem sources bill-vs-write-off
correctly. Crash safety needs **no journal**: a partially-placed gate's legs are unstarted escrows
reclaimed per-budget by the `SweepUnbound` janitor (§13.2) — the roll-back bias, unlike transfer's
complete-forward.

**Naming the sources from `sbatch` (#98).** A submission expresses its ordered funding list in
`--comment` as `obol-sources=a,b,c` (a keyed token, so other comment text is preserved). The shim
(`job_submit.lua`) parses it into `GateRequest.Sources`; absent → the job funds from its single
`--account`, exactly as before. `--comment` is user-editable, but this grants no privilege: the
daemon authorizes **every** source, so a submitter can only draw from accounts they already have
access to — the same authorization as `--account`. The daemon owns all split policy; the shim only
forwards the list. (The token requires no spaces around the `=`/commas; that keeps the parse a
single unambiguous field.)

**Job arrays split by WHOLE TASKS (#96).** A multi-source array funds in units of one task's cost
(`c·w`): source 1 funds the first `k₁` task indices, source 2 the next `k₂`, … with `kᵢ =
floor(Bᵢ/(c·w))` capped by the remaining count. Each leg is an ordinary single-budget ARRAY escrow
over a contiguous global task-index range, so the kernel is unchanged and each budget conserves on
its own. All-or-nothing: reject if `Σ floor(Bᵢ/(c·w)) < N`. Unlike the 1:1 time-slice legs (which
fan every BIND/SETTLE across all legs), array BIND/SETTLE for a given task route to the ONE leg
whose range owns that index, settling that leg's task at its local index; the master routing drops
when the last task across all legs settles. (Slice-each-task — every source funding part of every
task's walltime — was considered and rejected as far heavier for no real gain.)

---

## 10. Per-generation verification checklist

The one thing that genuinely varies across 22.05 / 23.11 / 24.05 and **must be verified per
generation before shipping**, because SchedMD states the Lua field set changes between versions:

- [ ] `admin_comment` is **readable** in the Lua `job_desc` table
      (grep `_get_job_req_field` in `src/plugins/job_submit/lua/job_submit_lua.c` for that version).
- [ ] `admin_comment` is **writable** from `slurm_job_submit`
      (grep `_set_job_req_field` in the same file). **Not yet confirmed for 22.05 specifically.**
- [ ] `tres_per_node|task|socket|job` and `time_limit` readable in `job_desc` (confirmed on master;
      verify per generation).
- [ ] `admin_comment` survives to the completion record visible to the daemon's tier-3 feed
      (jobcomp/slurmdbd).
- [ ] `site_factor` is available and loads under `priority/multifactor` for that version.
- [ ] The completion/node-fail feed mechanism (jobcomp plugin vs. slurmdbd record) and its
      reliability for that version — this determines janitor sweep frequency.

burstlab's generation structure is built to absorb exactly this: one verified shim per generation,
the rest shared.

---

## 11. Config changes the overlay makes

Shipped as a burstlab generation overlay (same pattern as the workloads track). Both target seams
are empty by default and confirmed unset in burstlab's templates:

- `JobSubmitPlugins=lua` (was unset) — installs `job_submit.lua`.
- `PriorityType=priority/multifactor` (default was `priority/basic`) plus the `site_factor` plugin —
  **this is the one config change with a real consequence:** enabling budget-burst flips the cluster
  from FIFO to multifactor. Stated explicitly so it is a deliberate choice, not a surprise.
- `obold` systemd unit (restart-always), its socket path, the per-partition policy table
  (including the shim's local `fail_closed` map), and the WAL/snapshot directory.

---

## 12. Mapping the seam to the proven kernel

Each Slurm event invokes a kernel transition that is already built and tested:

| Slurm event | Kernel transition |
|-------------|-------------------|
| `job_submit` (single) | `Submit(token, w, now)` |
| `job_submit` (array `%N`) | `SubmitArray(token, n, w, now)` |
| `site_factor` pending→running | `Start(jobid, now)` / `StartTask(...)` (burst reserve) |
| completion (normal) | `Complete(token, runtime, now)` |
| completion (hit walltime) | `Timeout(token, now)` |
| scancel | `Cancel(token, elapsed, now)` |
| `NODE_FAIL` / preempt | `InfraFail(token, elapsed, now)` (flag routes bill vs writeoff) |
| period boundary | `Lapse()` (admin-driven, between-windows) |
| admin add money | `TopUp(amount, now)` (raises B and B0 together) |
| admin remove money | `Withdraw(amount, now)` (lowers B and B0; caps at available B) |
| admin move money | `obol transfer` → `Withdraw` + `TopUp`, journaled (§12.1) |
| crash recovery | WAL replay through the same transitions |
| lost completion / started-orphan-after-crash | `SweepOrphans(liveKeys, policy, now)` — sweeps STARTED escrows absent from the live set; driven by the `reconcile` verb (#97) fed live job ids from `squeue` |
| never-bound token (submit→start crash) | `SweepUnbound(ttl, now)` — full refund of stale never-started escrows |

The seam adds no new money or burst logic — it only routes real events onto transitions whose
conservation and concurrency properties are already proven.

### 12.1 Atomic transfer between two budgets (`obol transfer`, #25)

Transfer is the **first operation to move money between two independent kernels**, each with its
own lock and WAL. Conservation (invariant #1) must hold across *both* budgets, not just each one.

A transfer is two legs: `from.Withdraw(amt)` then `to.TopUp(amt)`. Each leg is individually
crash-safe via its own WAL, but a crash *after* the withdraw's `fsync` and *before* the topup's
would **destroy** the in-flight amount. Two mechanisms close that window:

- **Per-leg WAL tag.** Each leg's logged command carries a transfer id (`Xfer`). `budget.HasXfer`
  reads a budget's WAL to ask "did the leg with this id commit?" — so recovery knows which legs
  landed, making each leg **exactly-once**.
- **Daemon journal.** Before the first leg, the daemon `fsync`s a small record
  (`<state-dir>/transfers/<id>.json`: `{from, to, amount, now}`). On restart, `recoverTransfers`
  reads leftover records and resolves each from what the two WALs show:
  - `withdrew && !deposited` → money left the source but never landed → **complete the deposit**.
  - `!withdrew` → no money moved (deposit never precedes withdraw) → **abort** (drop the record;
    do *not* re-run the withdraw, which a concurrent submit could now make impossible).
  - `!withdrew && deposited` → impossible ordering → **corruption, surfaced loudly**.

The two legs run **sequentially** (withdraw fully commits before topup begins), so only one kernel
lock is ever held at a time — no lock-ordering deadlock. The journaled `now` replays both legs,
preserving pure-`(state, command, now)`. Withdraw caps at *available* `B`, so reserved/consumed
money committed to live work can never be moved out from under it.

---

## 13. Known gaps (the honest list)

1. **`admin_comment` writability on 22.05** is unconfirmed — the per-generation checklist's first
   blocker for Gen 1.
2. **Submit→start orphan window** — *resolved (issue #15).* An escrow minted at the GATE but
   never bound to a job id (the daemon crashed in the submit→start gap) is marked `Started ==
   false` and carries its submit-time clock (`Escrow.Submitted`, persisted in the snapshot). The
   `SweepUnbound(ttl, now)` janitor reclaims any never-started escrow older than `ttl` with a
   full refund (it provably never ran). `obold` drives it on a ticker (`-unbound-ttl`,
   `-sweep-interval`). This is the one orphan class the jobid-based `SweepOrphans` cannot match.
   Its complement — a STARTED escrow whose job vanished (lost completion, or a crash that lost the
   daemon's routing) — is now handled too: *resolved (issue #97).* The `reconcile` admin verb takes
   the live Slurm job-id set (`squeue -h -o %A | obol reconcile`), the daemon maps it to live
   escrow keys, and `SweepOrphans` full-refunds the started escrows no longer live. The two
   janitors partition the orphan space by `Started`, so they never race.
3. **Group commit** — *resolved (issue #6).* `Append` writes the record to the page cache and
   returns; a background committer batches `fsync`s off the caller's path (while one slow `fsync`
   is in flight, more appends accumulate and the next `fsync` covers them all). The torn-tail
   discipline is preserved — a crash before the `fsync` loses the un-synced tail and the caller's
   still-in-memory mutation together. `Flush`/`Close` are synchronous durability barriers.
4. **Config durability** — *resolved (issue #8).* Config (cost rate, window, policy flags) is set
   at creation, captured in the snapshot, and **immutable thereafter**; it is durable as-is, proven
   by a recovery test. If mutation is ever added it must be a logged command applied through the
   replay path (never snapshot-only), to preserve the pure-`(state, command, now)` invariant.
5. **Hierarchy / multi-budget resolution** — §9 describes it; the kernel today proves only the
   single-budget core. Real model scope still to re-expand.
6. **Tier-2 read path** — *resolved (issue #7).* Every mutation publishes an immutable
   `ReadView{B, burstPot, rLive}` under the lock it already holds; `ReadSnapshot()` loads it
   lock-free via an atomic pointer, so the site_factor priority/burst reads (O(pending) per cycle)
   never take — and never contend — the gate write mutex. The published triple is internally
   consistent (one locked moment), which three independent atomics could not guarantee.

Items 1–3 gate a first Gen 1 deployment. Items 4–6 are real but can follow a working money-gate MVP.
