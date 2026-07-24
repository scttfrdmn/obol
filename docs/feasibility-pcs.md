# Feasibility: obol on AWS PCS (managed Slurm)

**Status:** feasible **with a seam redesign** — the GATE cannot attach as obol ships
today, but a documented PCS-supported hook path exists. This is the genuine
feasibility question (contrast [`feasibility-parallelcluster.md`](feasibility-parallelcluster.md),
which is feasible unchanged).
**Scope:** design/feasibility only; no deploy tooling ships in this doc.
**Sources:** the current (July 2026) AWS PCS documentation — the `SlurmCustomSetting`
API type and the cluster / queue / compute-node-group **allowlists**, and the
"Configure Slurm CLI Filter Plugins on an AWS PCS cluster" guide — plus obol's own
[`SEAM_DESIGN.md`](SEAM_DESIGN.md). Where a claim rests on live AWS docs it's marked
✅; where it rests on obol behavior that hasn't been run against PCS it's marked ❓.

---

## TL;DR

AWS PCS is **managed Slurm**: AWS owns and operates the `slurmctld` controller.
You do **not** get root or a shell on it, and you cannot set arbitrary `slurm.conf`
values — only an **allowlisted subset**, via the `SlurmCustomSetting`
(parameterName / parameterValue) API on `CreateCluster` / `UpdateCluster` /
`CreateQueue` / `CreateComputeNodeGroup`.

That breaks two of obol's three seam attachment points as shipped:

| obol seam hook | PCS allowlist? | Consequence |
|----------------|:--------------:|-------------|
| `JobSubmitPlugins=lua` (**GATE**) | ❌ **not allowed** | the submit gate can't attach the way it does today |
| `Prolog` + `PrologFlags` (**BIND**) | ✅ allowed (cluster level) | BIND can attach |
| `JobCompType=jobcomp/script` + `JobCompLoc` (**SETTLE**) | ❌ **not allowed** | the completion feed can't attach as a jobcomp script |

PCS **does** allowlist **`CliFilterPlugins=cli_filter/lua`**, a client-side submit
hook that can reject jobs, so a *reservation* gate can attach (GATE → `cli_filter`,
BIND → `Prolog`, SETTLE → `Epilog`/slurmdbd, `obold` on a customer login node — see
below).

**But the load-bearing question is now resolved, and it's the hard part:** a
`cli_filter` gate is **advisory, not enforceable** on PCS. AWS's own docs say
`cli_filter` *"can be easily bypassed by any user and must not be used for
security-critical policies,"* PCS does **not** support the controller-side
`job_submit` plugin that would enforce it, and PCS documents a bypass workflow
(submit from a standalone in-VPC instance with a BYO `slurm.conf` over the
controller's port-6817 ENI). See [Enforcement integrity](#enforcement-integrity--resolved-a-cli_filter-gate-is-bypassable-on-pcs).

So obol on PCS has **two honest shapes**, and picking between them is a product
decision (see [Recommendation](#recommendation)):

- **Advisory gate (Option A):** the `cli_filter` reserve/settle escrow — great UX and
  honest-case enforcement, bypassable by a determined user. Cheap; reuses the shipped
  shell-out transport (#137).
- **Limit projector (Option B, hard):** obol projects each account's remaining budget
  onto a **QOS/association limit** the managed controller enforces on every job from
  any client — unbypassable, but coarser (cumulative caps, not per-job escrow) and a
  new enforcement model.

This is a real redesign of *where* the gate runs, *how* the shim reaches obold, and —
newly — *what "enforcement" even means* on a managed controller. It is feasible; it is
the work; and the enforcement model must be chosen before building.

---

## What PCS gives you and takes away

PCS provisions and operates the controller as a managed service. From the AWS docs:

- **Customization is an allowlist.** "AWS PCS supports a subset of Slurm settings."
  Settings "that could compromise service account security or interfere with managed
  service capabilities are restricted." You set them as
  `SlurmCustomSettings=[{parameterName=…,parameterValue=…}]` on the cluster, queue,
  or compute-node-group APIs. ✅
- **No shell/root on the controller.** The managed controller is not a host you log
  into. This is the fundamental difference from ParallelCluster. ✅ (by absence — the
  service exposes no controller SSH)
- **You do own login nodes and compute nodes.** These run on AMIs you influence and
  can carry files you deploy (AMI / S3 / shared filesystem) — which is exactly where
  the `cli_filter.lua` script and a customer-side `obold` can live. ✅

### The cluster-level allowlist (relevant entries)

Present ✅ (obol can use): `Prolog`, `PrologFlags`, `Epilog`, `TaskProlog`,
`TaskEpilog`, `CliFilterPlugins`, `CliFilterParameters`, `SchedulerParameters`,
`AccountingStorageType`/`AccountingStorage*`, the `Priority*` weights, `Preempt*`,
`SelectTypeParameters`.

Absent ❌ (obol relies on these today): **`JobSubmitPlugins`**, **`JobCompType`**,
**`JobCompLoc`**, `PrologSlurmctld`, `EpilogSlurmctld`. The `*Slurmctld` variants are
absent because they'd run *on the managed controller* — precisely what PCS won't let
you inject.

### The queue-level allowlist

`AllowAccounts`, `AllowQoS`, `Default`, `DefaultTime`, `DenyAccounts`, `DenyQoS`,
`ExclusiveUser`, `GraceTime`, `MaxTime`, `OverSubscribe`, `OverTimeLimit`,
`PowerDownOnIdle`, `PreemptMode`, `PriorityJobFactor`, `PriorityTier`, `QOS`,
**`TRESBillingWeights`**. No prolog/submit/jobcomp here (those are cluster-level).

### The compute-node-group allowlist

`CpuSpecList`, `Features`, `MemSpecLimit`, `Parameters` (Slurm ≥25.11), `RealMemory`,
`Sockets` (≥25.11), `Weight`. Nothing seam-relevant — this level is node shape only.

---

## The `cli_filter/lua` path (the crux)

AWS documents `CliFilterPlugins` as a first-class supported custom setting, with its
own configuration guide. The salient facts, quoted/paraphrased from that guide:

- **Enable it** by setting `CliFilterPlugins=cli_filter/lua` in `slurmCustomSettings`
  at `CreateCluster` (or add via `UpdateCluster` without recreating). ✅
- **Deploy your Lua script** to `/etc/aws/pcs/scheduler/slurm-{version}/cli_filter.lua`
  on **all instances in the cluster** (login + compute nodes), via AMI, S3, or a
  shared filesystem. From Slurm 25.11 the path is configurable via
  `cli_filter_lua_path` in `CliFilterParameters` (default
  `/etc/aws/pcs/scheduler/slurm/cli_filter.lua`). ✅
- **What it does:** "Job submissions trigger your custom validation logic **before
  reaching the Slurm controller**"; "**Non-compliant jobs are rejected with your
  custom error messages**"; compliant jobs proceed normally. ✅

That is a submit-time gate that can say **no** with a message — functionally what
obol's GATE needs. But it differs from `job_submit/lua` in ways that matter:

1. **It runs client-side, on the submitting node**, not on the controller. So the
   thing it calls (`obold`) must be reachable **from the login/compute node**, not
   from the controller. ❓ (design implication, confirmed by the "runs before
   reaching the controller" wording)
2. **It's the `sbatch`/`srun`/`salloc` CLI's own filter** — it can mutate/deny the
   request, and it can read/set `job_desc` fields including (subject to Slurm
   version) `admin_comment`. Whether obol's correlation-token stamp into
   `admin_comment` works the same from `cli_filter` as from `job_submit` on the
   target PCS Slurm version (25.11) is **unverified**. ❓
3. **A client-side filter is advisory to a determined user** in a way a controller
   plugin is not: `cli_filter` runs in the user's `sbatch` process. A user who
   submits through a *different* client that doesn't load the filter, or who talks to
   the controller's RPC directly, bypasses it. On the managed controller you can't
   add a `job_submit` backstop. **This is the load-bearing open question** — see
   Enforcement integrity below. ❓

---

## Recommended attachment model (PCS)

### GATE — `cli_filter/lua` on login/compute nodes, calling a login-node `obold`
Ship obol's gate logic as a `cli_filter.lua` deployed to the PCS-mandated path on
every submitting instance. It makes the same one-shot round-trip to `obold` the
current `job_submit.lua` does — but to an `obold` running on a **customer-owned login
node**, reached over a Unix socket *on that node* (or a local TCP/UDS depending on
where submits originate). Rejection returns the gate's "no" as the CLI error.

### BIND — `Prolog` (allowlisted)
`Prolog` + `PrologFlags=Alloc` are on the cluster allowlist, so the BIND step (bind
token ↔ jobid at job start) attaches essentially as it does today. The prolog runs on
the compute node at allocation; it must reach the same `obold`.

### SETTLE — `Epilog` (allowlisted), not `jobcomp/script`
`JobCompType`/`JobCompLoc` are **not** allowlisted, so obol's current jobcomp-script
SETTLE can't attach. `Epilog` **is** allowlisted — re-home settlement onto the epilog
(runs on the compute node at job end; reach `obold`, settle actual runtime, refund
the tail). Alternatively, since `AccountingStorage*` is allowlisted, a slurmdbd-driven
completion feed (#13) becomes the more robust SETTLE path on PCS specifically.

> **Resolved (Jul 2026): there is no EventBridge job-completion feed to lean on.**
> The EventBridge schema registry shows PCS emits only CloudTrail-derived events
> (`aws.pcs@AWSAPICallviaCloudTrail`, `…AWSServiceEventviaCloudTrail`, console/sign-in)
> — control-plane API/service events, **no job-lifecycle event**. So a "settle from an
> EventBridge job-completion event" design is off the table; SETTLE on PCS is `Epilog`
> (with the node-fail caveat — an epilog is skipped when the node dies, so infra-fail
> settlement leans on the unbound/`reconcile` janitors) and/or the slurmdbd feed.
> `EpilogSlurmctld` (controller-side, fires even on node failure) is **not** allowlisted.

### obold — on a customer login node, not the controller
The single biggest structural change: **`obold` cannot run next to `slurmctld`**
because there is no customer access to the managed controller. It runs on a
customer-owned login node. Implications to work through (❓):
- **The transport is simpler on PCS than on the controller, not harder.** obol's
  in-process Lua socket transport (`obol_wire.lua` + `obol_transport.lua`, needing
  luasocket/FFI) exists *only* to keep the `job_submit` GATE off the slurmctld
  scheduler lock (SEAM_DESIGN §1: must not fork on the lock). BIND and SETTLE already
  just exec the `obol` binary. On PCS the GATE is a **`cli_filter/lua`** that runs
  **client-side in the user's `sbatch`, on no lock** — so it can simply **shell out to
  `obol gate`** like BIND/SETTLE do, with no Lua socket backend at all. **This
  shell-out transport already shipped** (#137, merged): `job_submit.lua` falls back to
  exec'ing `obol gate` when no in-process backend loads. A PCS `cli_filter.lua` reuses
  that same exec path as its primary transport — so the gate mechanism itself needs no
  new work; only the `cli_filter` wrapper + cross-host addressing below remain.
- **But the CLI must reach `obold` across hosts.** If the submit login node isn't
  where `obold` runs, `obol gate` needs `--addr host:port` against an `obold` **TCP**
  listener (both are UDS-only today). This is a real code change — and it collides
  with enforcement/identity below (a TCP peer has no `SO_PEERCRED`), so transport and
  authorization are **one** decision on PCS, not two.
- State durability moves to the login node's storage (EBS or shared FS), same
  single-writer caveat as the ParallelCluster case
  ([`feasibility-parallelcluster.md`](feasibility-parallelcluster.md)).

### Partition policy still fits
`TRESBillingWeights` is allowlisted per-queue, and obol's per-partition fail-open/
fail-closed lives in the shim (now `cli_filter.lua`), so the cloud-partition
fail-closed policy still applies — PCS partitions are cloud/rented, so fail closed.

---

## Enforcement integrity — RESOLVED: a `cli_filter` gate is bypassable on PCS

obol's core promise is a **hard gate**: a job that can't be funded **does not
schedule**. On self-managed Slurm and ParallelCluster that's guaranteed because the
gate is a **controller-side** `job_submit` plugin — every submission path goes
through it, regardless of the client.

**On PCS this guarantee cannot hold for a `cli_filter` gate — and this is now settled,
by AWS's own documentation, not inference:**

- **`cli_filter` is explicitly declared not a security boundary.** AWS: *"CLI Filter
  Plugins can be easily bypassed by any user and must not be used for security-critical
  policies. Users can disable CLI Filter Plugins by providing a custom configuration
  that has `CLIFilterPlugins` disabled while submitting jobs."*
  ([PCS: CLI filter plugins](https://docs.aws.amazon.com/pcs/latest/userguide/slurm-cli-filter-plugins.html))
  This matches SchedMD: cli_filter *"is entirely executed client-side"* and *"must not
  be relied upon for security purposes."*
- **The controller-side backstop is unavailable.** AWS: *"Does AWS PCS support Slurm
  Job Submit Plugin? No … Use CLI Filter Plugin instead."*
  ([PCS: cli_filter FAQ](https://docs.aws.amazon.com/pcs/latest/userguide/slurm-cli-filter-plugins-faq.html))
  So the one hook that *is* enforceable (`job_submit`, server-side, runs for every
  `sbatch`/`salloc`/`slurmrestd` path) can't be installed.
- **The bypass is a documented workflow, not a theoretical hole.** PCS documents
  submitting from a **standalone, customer-owned EC2 instance "outside of AWS PCS
  management"**: install a compatible Slurm, fetch the cluster auth key from Secrets
  Manager to `/etc/slurm/slurm.key`, point `sackd` at the `SLURMCTLD` endpoint (the
  cross-account ENI on **port 6817**, reachable through a **customer-controlled**
  security group), and submit. Such a client carries its own `slurm.conf` and never
  loads the `cli_filter`. ([standalone login nodes](https://docs.aws.amazon.com/pcs/latest/userguide/working-with_login-nodes_standalone.html),
  [networking/SGs](https://docs.aws.amazon.com/pcs/latest/userguide/working-with_networking_sg.html))

So a `cli_filter` budget gate on PCS is **advisory** — good for fast feedback and the
honest-majority case, but a determined user can route around it. It is **not** the hard
gate obol gives elsewhere, and no amount of `cli_filter` engineering changes that.

### The only enforceable lever on PCS: the accounting/QOS layer

What PCS *does* enforce controller-side is the **slurmdbd accounting/limits layer**,
and its controls are allowlisted in `SlurmCustomSetting`: `AccountingStorageEnforce`,
QOS + associations (`QOS`/`AllowQoS`, `AllowAccounts`/`DenyAccounts`), and partition
limits (`MaxTime`, `EnforcePartLimits`, `TRESBillingWeights`). The viable **hard**
model on PCS is therefore *not* an escrow gate at submit but a **limit projector**:

> obol stays the source of truth for money, and continuously projects each account's
> remaining budget onto a Slurm **QOS/association limit** (e.g. `GrpTRESMins`, or a
> per-account cap) that the managed controller enforces on every job, from any client.

This is a genuinely different enforcement model from obol's reserve/settle escrow — it
trades submit-time precision (exact per-job reservation) for controller-side
unbypassability, and it's coarser (limits are cumulative caps, not per-job holds).
Whether obol should offer a "PCS mode" built on limit projection, keep `cli_filter` as
an advisory-only gate, or both, is a **product decision** — captured in the
recommendation below and gated on it before any build.

The `cli_filter` path still has value on PCS as the **UX/reservation layer** (instant
"you can't afford this" feedback, and the escrow bookkeeping for honest submits), as
long as it's paired with the limit projector for the actual enforcement — never sold
as the enforcement itself.

---

## Confirmed vs. unknown

### Confirmed ✅ (from live July-2026 AWS docs)
- PCS customization is an **allowlist** via `SlurmCustomSetting`
  (parameterName/parameterValue) at cluster/queue/compute-node-group scope.
- `JobSubmitPlugins`, `JobCompType`, `JobCompLoc` are **not** in the allowlist.
- `Prolog`, `PrologFlags`, `Epilog`, `CliFilterPlugins`, `CliFilterParameters`,
  `AccountingStorage*`, `TRESBillingWeights` **are** allowlisted.
- `CliFilterPlugins=cli_filter/lua` is supported, deploys `cli_filter.lua` to
  `/etc/aws/pcs/scheduler/slurm-{version}/`, runs **before the controller**, and can
  **reject jobs with a custom message**.
- No customer shell/root on the managed controller.
- **PCS has no job-lifecycle EventBridge event** (verified via the EventBridge schema
  registry — only CloudTrail-derived control-plane events exist). So SETTLE can't ride
  an event feed; it's `Epilog` and/or slurmdbd.
- **obol's GATE transport is a solved problem for PCS**: the `cli_filter` primary path
  is the already-shipped shell-out to `obol gate` (#137); no Lua socket backend needed.
- **Slurm 25.11 is now under obol CI** (the `managed` multi-gen lane, #141) and a live
  ParallelCluster ran the seam on 25.11.4 — so the version PCS ships is exercised.

### Unknown / to confirm ❓
1. ~~Enforcement integrity~~ **RESOLVED (above):** a `cli_filter` gate is bypassable
   per AWS's own docs, and `job_submit` (the enforceable hook) is unsupported. The
   only controller-side enforcement is the accounting/QOS layer → obol's hard-gate
   model on PCS must be a **limit projector**, not a submit escrow. This flips the
   feasibility answer from "gate with a caveat" to "advisory gate **or** a different
   (limit-projection) enforcement model" — a product decision, see Recommendation.
2. **`obold` off-host transport + identity.** The GATE itself is easy on PCS
   (`cli_filter` shells out to `obol gate` — no Lua socket backend needed, #137). The
   open piece is reaching a login-node `obold` when the submit/prolog/epilog runs
   elsewhere: that needs a **TCP** listener (obold + CLI are UDS-only today), and a
   TCP peer has **no `SO_PEERCRED`**, so the admin/funding-source authorization that
   the socket identity provides today must move onto an explicit authenticated
   channel (token/mTLS). Transport and authorization are the same change.
3. **`admin_comment` writability from `cli_filter`** on PCS Slurm 25.11 for the
   correlation token, or a fallback carrier.
4. **SETTLE robustness** — with no EventBridge feed (resolved above) and no
   controller-side `EpilogSlurmctld`, node-failure settlement leans on `Epilog` +
   the unbound/`reconcile` janitors, or the slurmdbd feed (#13). Which combination is
   robust on PCS needs a prototype; not a blocker (the janitors already backstop lost
   settlements).
5. **Where obold's state lives** and single-writer fencing on a login node
   (mirrors the ParallelCluster durability unknowns).

Resolved since the first draft: the GATE transport (was #2's hard part — now the
shipped shell-out, #137), the SETTLE event feed (there isn't one — use Epilog/slurmdbd),
and Slurm-25.x coverage (now the `managed` CI lane, #141).

---

## What obol would want from PCS, ideally

Everything above is obol adapting to PCS as it exists today. This section is the
inverse: the short list of things AWS PCS could expose that would let obol be a
**clean, hard budget gate** on a managed controller instead of a "gate with a
caveat." It doubles as concrete feedback to the PCS team. Roughly in priority order:

1. **A controller-side submit gate hook — the one thing that matters most.**
   Allowlist `JobSubmitPlugins=lua` (even a constrained/sandboxed form), *or* offer a
   first-class "admission webhook": before a job is accepted, PCS calls a
   customer-provided endpoint that can reject with a message. Either makes the gate
   **unbypassable** — the whole enforcement-integrity question disappears, because the
   decision no longer lives in the user's client. This is the difference between a
   hard gate and an advisory one.
2. **A settlement / job-completion event.** Allowlist `JobCompType=jobcomp/script` (or
   `jobcomp/kafka`/HTTP, or an EventBridge job-lifecycle event carrying jobid + final
   state + elapsed). obol needs a reliable "job ended, here's the actual runtime" feed
   to settle escrows; today it must lean on `Epilog` (compute-node, skipped on node
   failure) or scrape slurmdbd.
3. **`PrologSlurmctld` (controller-side start hook), or the job's `admin_comment`
   readable/writable from `cli_filter`.** obol correlates the submit-time reservation
   to the running job via a token in `admin_comment`. A controller-side start hook (as
   on ParallelCluster) or a guarantee that `cli_filter` can stamp/read `admin_comment`
   on PCS's Slurm makes BIND clean; otherwise the token needs a fallback carrier.
4. **A documented, private endpoint to reach a customer sidecar.** obol's daemon holds
   the money state and wants a stable, authenticated way for the gate/settle hooks to
   call it (a per-cluster private DNS name + a workload-identity token, say). This
   removes the "TCP listener with no `SO_PEERCRED`" problem — the identity comes from
   the platform, not the socket.
5. **A way to close the submit surface.** Today PCS documents submitting from a
   standalone in-VPC instance with a BYO `slurm.conf` (the bypass). A cluster setting
   that restricts submission to PCS-managed clients carrying the `cli_filter` would
   close the bypass by construction — a weaker but simpler alternative to (1).

(1) alone would make PCS a first-class obol target. (1)+(2)+(3) would make it
*equivalent* to a self-managed cluster. Absent (1), obol on PCS is either an
**advisory** `cli_filter` gate or the **limit-projector** hard model (QOS/association
limits the controller enforces) described under Enforcement integrity — the two honest
options, since `cli_filter` alone is confirmed bypassable.

---

## Recommendation

The feasibility-defining question is **answered**: on PCS a `cli_filter` gate is
**advisory, not enforceable**, and the enforceable lever is the accounting/QOS layer.
So this is no longer "sequence an investigation" — it's a **product fork to decide
before building**:

- **Option A — advisory `cli_filter` gate.** Ship the reserve/settle escrow via
  `cli_filter` (GATE) + `Prolog`/`Epilog` (BIND/SETTLE), reusing the shipped shell-out
  transport (#137). Honest framing: it enforces for cooperative submits and gives
  instant feedback, but a determined user can bypass it. Cheapest; matches obol's
  existing model; but it is *not* a hard gate and must never be sold as one.
- **Option B — limit projector (hard).** obol keeps the money truth and continuously
  projects each account's remaining budget onto a controller-enforced QOS/association
  limit (`GrpTRESMins`/caps) via `SlurmCustomSetting`. Unbypassable, from any client —
  but coarser (cumulative caps, not per-job escrow) and a new enforcement model to
  build and reason about (conservation still lives in obol; the projected limit is a
  derived, lossy view).
- **Option A+B.** `cli_filter` as the UX/reservation layer *and* the limit projector
  for enforcement. Best experience + hard enforcement, most work.

**Recommended: decide A vs. A+B with the project owner before writing PCS code.** My
lean is **A+B eventually, A first** — ship the advisory gate (small, reuses #137,
delivers the UX and honest-case value) explicitly labeled advisory, and design the
limit projector (Option B) as the enforcement upgrade. Do **not** build B blind: it
needs its own design note (how a lossy cumulative cap coexists with exact escrow
conservation — an invariants question for the kernel owner).

Once the fork is chosen, the buildable work is:
1. **Off-host transport + identity** (unknown #2): a TCP `obold` listener + an
   authenticated channel (no `SO_PEERCRED` across hosts). Needed by both options; the
   one clear code change. GATE transport itself is done (#137).
2. **`integ-pcs` tier** against a real PCS cluster (analogous to `integ-pcluster`,
   same no-destructive rule — attach to an existing cluster, never `create-cluster`).
3. **(Option A) cli_filter wrapper + Epilog/slurmdbd SETTLE**; **(Option B) the limit
   projector + its design note.**
4. **In parallel, ask AWS for a controller-side gate hook** ("What obol would want
   from PCS" #1) — it would collapse this whole fork into a clean hard gate and make
   Option A the complete answer.

None of this touches the money kernel's invariants
([`../CLAUDE.md`](../CLAUDE.md)) — Option B adds a *derived* projection, not a new
money path. Tracked under the **AWS PCS attachment** milestone.
