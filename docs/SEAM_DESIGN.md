# Slurm Budget Plugin — Seam Design

How the proven budget kernel attaches to a running Slurm cluster.

## Status

Two things exist at different maturities:

- **The kernel** (this repo, Go): money ledger with an atomic submit-gate, escrow/refund,
  per-partition policy flags, lapse, job arrays, and an explicit burst token bucket — all
  holding conservation and non-negativity under the race detector, with crash-safe durability
  (command-logged WAL + snapshot recovery + orphan janitor). This is **built and tested**.
- **The seam** (this document): how that kernel meets Slurm's plugin surface. This is
  **designed, not yet built**. The Go daemon described here is the next implementation step;
  the Lua and `site_factor` pieces require a real cluster (burstlab) to validate.

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
- Hierarchy auto-resolves to the most specific account and rolls up (child spend debits ancestors);
  explicit disambiguation is reserved for unrelated/sibling budgets. (Hierarchy rollup is deferred
  model scope — the kernel today proves the single-budget core.)

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
| crash recovery | WAL replay through the same transitions |
| lost completion | `SweepOrphans(liveIDs, policy, now)` + unbound-token TTL |

The seam adds no new money or burst logic — it only routes real events onto transitions whose
conservation and concurrency properties are already proven.

---

## 13. Known gaps (the honest list)

1. **`admin_comment` writability on 22.05** is unconfirmed — the per-generation checklist's first
   blocker for Gen 1.
2. **Submit→start orphan window** — handled by an unbound-token TTL, which is designed but not
   yet built into the janitor.
3. **Group commit** — the off-lock durability batching is designed (§3) but not yet implemented;
   the current WAL `fsync`s under its own lock.
4. **Config durability** — *resolved (issue #8).* Config (cost rate, window, policy flags) is set
   at creation, captured in the snapshot, and **immutable thereafter**; it is durable as-is, proven
   by a recovery test. If mutation is ever added it must be a logged command applied through the
   replay path (never snapshot-only), to preserve the pure-`(state, command, now)` invariant.
5. **Hierarchy / multi-budget resolution** — §9 describes it; the kernel today proves only the
   single-budget core. Real model scope still to re-expand.
6. **Tier-2 read path** — the copy-on-write snapshot for lock-cheap priority reads is designed but
   the kernel currently serves all reads under the one write mutex.

Items 1–3 gate a first Gen 1 deployment. Items 4–6 are real but can follow a working money-gate MVP.
