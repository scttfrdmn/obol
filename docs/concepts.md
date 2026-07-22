# Obol concepts

This guide builds up how Obol works one idea at a time, starting from a single
worked example. It's the "why and what" companion to the architecture in
[`SEAM_DESIGN.md`](SEAM_DESIGN.md) and the deployment steps in the
[README quickstart](../README.md#quickstart) / [`INTEGRATION.md`](INTEGRATION.md).

If you take away one sentence: **Obol reserves a job's projected cost at submit,
admits it only if the account can afford it, then bills actual usage and refunds
the rest.** Everything below is a layer on that.

---

## 1. The core loop: reserve → admit → settle → refund

A budget is a pot of **money** with a **cost rate**. When a job is submitted, Obol
computes its worst-case cost (`rate × requested walltime`), reserves that amount,
and admits the job only if the pot can cover it. When the job ends, Obol bills the
*actual* runtime and returns the unused reservation.

Worked example — a lab funded **$10,000**, cost rate **$1 per node-hour**:

| Step | Action | Available | Reserved | Consumed |
|------|--------|-----------|----------|----------|
| start | lab is funded | $10,000 | $0 | $0 |
| submit | Alice submits a 4-node job, requested 80 node-hours → est. **$320** | $9,680 | $320 | $0 |
| run | job dispatches and runs | $9,680 | $320 | $0 |
| finish | it finishes early — actual **$241.60** | $9,758.40 | $0 | $241.60 |

At submit the $320 moves from *available* to *reserved* (the gate). At finish the
$320 reservation is dissolved: $241.60 becomes *consumed* (billed), and the
**$78.40** tail returns to *available*. A fifth job that would need more than the
$9,758.40 now available is **rejected at `sbatch`** and never schedules.

That "rejected before it runs" is the whole point — see [§9](#9-how-obol-relates-to-slurm-accounting).

### Why money is integer units

Obol stores money as **integer units** (`int64`), never floating point. This is a
deliberate correctness decision, not an implementation detail:

- **Floats can't represent money exactly.** `0.1 + 0.2 != 0.3` in binary floating
  point. Summing thousands of reservations, charges, and refunds in floats
  accumulates rounding error, so the books would only *approximately* balance —
  and you'd have to compare them with a fuzzy epsilon.
- **Integers make conservation *provable*.** Obol's core invariant is
  `B0 == available + reserved + consumed + write_off` — checked with exact `==`
  after every transition ([§8](#8-conservation-the-one-invariant)). That equality
  is only meaningful, and only holds under concurrency and crash-replay, because
  every add and subtract is exact. Floats would make the one property a
  money-enforcement tool must guarantee unverifiable.
- **Cost is `rate × walltime`, both integers**, so a reservation and every
  split of it (billed vs. refunded) is an exact integer with no residue to lose.

So Obol has no notion of "dollars" or "cents" of its own — it counts **units**,
and *you* choose what a unit is worth. Pick one and use it consistently across the
config: if you want cent precision, let **1 unit = $0.01**, and the $10,000 lab
above is `1,000,000` units funded, `32,000` reserved, `24,160` consumed. Want
whole-dollar accounting? Let 1 unit = $1. The rate is likewise integer units per
second of walltime (the config lets you write it per hour/minute and validates
that it divides evenly into a whole per-second rate, so it stays exact).

---

## 2. Balance vs. reservation vs. consumed

Every unit of a budget is always in exactly one of four places:

- **Available (`B`)** — spendable right now; what the gate checks against.
- **Reserved** — held for live jobs (Σ of each live job's reservation).
- **Consumed** — billed to the user for runtime that actually happened.
- **Write-off** — runtime the *system* absorbed instead of billing (see
  [§7](#7-partition-policy-billed-vs-written-off)).

The original allocation is `B0`. At all times
`B0 == available + reserved + consumed + write_off`. A reservation is not a
charge — it's a hold that is later split into consumed + refund.

`obol show --account <name>` prints all of these plus the burn rate and
time-to-empty.

---

## 3. Cost: flat rate, TRES weights, or node-type pricing

The gate needs a **rate** (units per second) to turn requested walltime into a
reservation. Obol resolves it in this order:

1. **Node-type worst-case** — if the job's partition has node types with prices
   configured, the gate reserves the *most expensive* node it could land on
   (placement isn't known yet), then **reprices down** to the actual node's rate
   when Slurm binds one (the `BIND` step). This is how a partition with mixed
   hardware charges correctly.
2. **TRES weights** — cost per allocated CPU / GPU / MB-second, if configured
   (rides Slurm's existing `tres_*` request fields).
3. **Flat rate** — the account's own per-second rate, the simple default.

Whichever applies, the rate is **frozen into the job's reservation at submit**, so
a later rate change never retroactively re-prices a running job.

---

## 4. Hierarchy & resolution: account → budget

A submission names a Slurm **account**; Obol resolves it to that account's budget
by **exact name match**. Obol reuses Slurm's account tree (from slurmdbd) rather
than maintaining a parallel identity store — but resolution is flat: a
sub-account with no budget of its own does **not** roll up to a parent, it simply
fails to resolve and the job is rejected as unfunded.

Access defaults to **trusting Slurm** — if slurmdbd already let the user submit
under an account, Obol funds it. An account may optionally set an
`allow_users` / `allow_groups` list, in which case Obol *additionally* checks the
submitter's identity (a kernel-verified uid, not a spoofable field). Only then is
that lookup incurred, keeping the common path free of it.

---

## 5. Time windows & lapsing

A budget optionally carries a **window** `[start, end)`. Submits are admitted only
while the budget is *active* and `now` is inside the window; outside it, the
budget has **lapsed** and new jobs are rejected. Live jobs that were already
admitted settle normally — a window closing under a running job never strands or
corrupts it; its reservation still resolves to consumed + refund.

Windows are what make "a grant that expires at quarter-end" or "this month's
allocation" expressible. `obol set-window` moves a window at runtime.

---

## 6. Banked burst: permission, not money

Burst lets an account run *wider than its steady pace* by banking idle capacity.
It is **permission, not money** — a separate bounded ledger that never touches the
money books.

- The **sustainable rate** `r0 = allocation / window` is the pace at which you'd
  spend the whole budget evenly over the window.
- Running *below* `r0` **banks permission tokens** (up to a ceiling, a configured
  fraction of the allocation).
- A job that pushes your *aggregate* running rate *above* `r0` must **spend banked
  tokens** to dispatch — `excess_rate × walltime` tokens. No tokens → it waits.

So a lab that runs quietly most of the month banks capacity, then near a deadline
dispatches a wide burst it "paid for" in advance — instead of being throttled to a
flat concurrency cap. Crucially, the job still costs its real **money** when it
settles; burst only shapes *when* it's allowed to start. Enable/adjust burst per
account (`burst_enabled`/`burst_ceiling_pct`/`burst_draw_cap`, or `obol set-burst`).

---

## 7. Partition policy: billed vs. written off

Not all runtime is the user's fault. When a job dies to a **node failure or
preemption**, a per-budget policy decides who pays:

- **cloud / owned-you-rented** → the user is *billed* the elapsed time (the money
  was really spent on cloud instances).
- **on-prem / owned** → the loss is *written off* by the system (the hardware was
  going to cost the same regardless).

This is the `write_off` bucket from [§2](#2-balance-vs-reservation-vs-consumed). A
clean completion or timeout always bills the user; only infrastructure failures
consult the policy.

---

## 8. Conservation: the one invariant

Because money is integer units, Obol holds this **exactly** — not approximately —
at every step:

```
B0 == available + reserved + consumed + write_off
```

Every transition (submit, dispatch, complete, cancel, node-fail, top-up,
withdraw, transfer, reprice) asserts it afterward. This is checked under the race
detector and replayed from a crash-safe log; a violation is treated as corruption,
not a rounding blip. It's what lets you trust that a budget's numbers are real —
the property a money-enforcement tool has to earn.

---

## 9. How Obol relates to Slurm accounting

Experienced Slurm operators will ask: *doesn't slurmdbd already do this?* The
answer is that Obol and Slurm's native machinery answer **different questions at
different times**, and Obol builds on Slurm rather than replacing it.

| | Slurm accounting/QOS/fair-share | Obol |
|---|---|---|
| **When** | during and *after* the job (`sacctmgr`, `sacct`) | *before* the job is admitted, at `job_submit` |
| **Question** | how much has this account used? how should it be ranked/throttled? | can this account **afford** this job right now? |
| **Unit** | service units / TRES-billing, GrpTRESMins caps | **money** (integer units), independent of SU |
| **On overrun** | reported after the fact; QOS/limits can cap in SU terms | the job **does not schedule** |
| **Identity** | associations, the account tree | **reuses** Slurm's account tree; adds no parallel store |

Concretely, Obol:

- **Gates before, doesn't measure after.** `TRESBillingWeights` + `sacct` tell you
  what a job *cost* once the compute is already gone. On a cloud-backed cluster
  that's a bill you can't unspend. Obol reserves and admits/denies *at submit*.
- **Denominates in money, not SUs.** GrpTRESMins and QOS limits cap resource-time;
  Obol caps *dollars*, which is what a grant or a cloud invoice is actually in.
- **Reuses the account tree.** Obol resolves the same `--account` and trusts
  slurmdbd's membership; it does not reimplement associations.
- **Complements fair-share.** Fair-share decides *ordering* among competing jobs;
  Obol decides *admissibility* by funding. A job can be both high-priority and
  unfunded — Obol rejects it; a job can be funded and low-priority — fair-share
  still orders it.

Think of it as the layer that turns "measured usage" into "enforced budget," at
the one moment enforcement is still possible.

---

## Where to go next

- **Deploy it:** [README quickstart](../README.md#quickstart) and
  [`INTEGRATION.md`](INTEGRATION.md) (a complete working single-node example lives
  in `test/docker/`).
- **Why the architecture looks the way it does:** [`SEAM_DESIGN.md`](SEAM_DESIGN.md)
  — the sidecar daemon, the three-tier latency model, the `admin_comment`
  correlation token, and the failure-mode handling.
- **The money rules that must never break:** the invariants in
  [`../README.md`](../README.md#design-invariants) and `CLAUDE.md`.
