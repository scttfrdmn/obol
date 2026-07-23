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

But there is a path. PCS **does** allowlist **`CliFilterPlugins=cli_filter/lua`**, a
client-side Slurm submit hook that AWS explicitly documents can **reject
non-compliant jobs before they reach the controller**. And `Prolog`/`Epilog` and
`AccountingStorage*` are allowlisted. So the feasible design is:

- **GATE → re-home onto `cli_filter/lua`** (client-side, on login/compute nodes).
- **BIND → `Prolog`** (unchanged in spirit).
- **SETTLE → `Epilog`** (instead of a jobcomp script), or a slurmdbd-driven feed.
- **obold → cannot live on the managed controller.** It runs on a **customer-owned
  login node** (you own those in PCS), and the seam reaches it over a Unix socket
  *there*, not on the controller.

This is a real redesign of *where* the gate runs and *how* the shim reaches obold —
not a small config change. It is feasible, but it is the work.

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

### obold — on a customer login node, not the controller
The single biggest structural change: **`obold` cannot run next to `slurmctld`**
because there is no customer access to the managed controller. It runs on a
customer-owned login node. Implications to work through (❓):
- The seam's socket transport (`obol_transport.lua`, today a **local** Unix socket)
  must reach `obold` from wherever `cli_filter`/prolog/epilog run. If submits happen
  on the same login node as `obold`, a local UDS still works; if submits/prologs run
  on compute nodes, the transport needs a network path (obol's transport is
  UDS-only today — this is a transport feature, not just config).
- State durability moves to the login node's storage (EBS or shared FS), same
  single-writer caveat as the ParallelCluster case
  ([`feasibility-parallelcluster.md`](feasibility-parallelcluster.md)).

### Partition policy still fits
`TRESBillingWeights` is allowlisted per-queue, and obol's per-partition fail-open/
fail-closed lives in the shim (now `cli_filter.lua`), so the cloud-partition
fail-closed policy still applies — PCS partitions are cloud/rented, so fail closed.

---

## Enforcement integrity — the load-bearing question

obol's core promise is a **hard gate**: a job that can't be funded **does not
schedule**. On self-managed Slurm and ParallelCluster that's guaranteed because the
gate is a **controller-side** `job_submit` plugin — every submission path goes
through it.

On PCS the gate must move **client-side** (`cli_filter`), and there is **no
controller-side backstop available** (`JobSubmitPlugins` is not allowlisted). A
`cli_filter` runs in the user's submit process. That raises a genuine question: **can
a user bypass the gate** by submitting through a client that doesn't load the filter,
or by hitting the controller RPC directly?

Mitigations to investigate (all ❓):
- **AccountingStorage / QOS as a second wall.** `AccountingStorage*`, `QOS`,
  `Allow/DenyAccounts`, and `TRESBillingWeights` are allowlisted. obol could pair the
  soft `cli_filter` gate with a *hard* association/QOS limit that PCS **does** enforce
  controller-side (e.g. a GrpTRESMins or a per-account limit obol keeps in sync). The
  gate becomes "obol decides, and expresses the decision as a Slurm limit the managed
  controller enforces." This is a different enforcement model than obol's escrow gate
  and needs design.
- **Locked-down submit surface.** If PCS login nodes are the *only* submit path and
  they all carry the filter (deployed per the mandated path on every instance), the
  bypass surface may be closed in practice. Confirm whether PCS permits submitting
  from arbitrary customer clients against the managed controller. ❓
- **Whether `cli_filter` can reach `admin_comment`** on PCS's Slurm (25.11) for the
  correlation token, or whether the token must move to `--comment`/a spank env. ❓

Until this is resolved, PCS obol is **"gate with a caveat"**, not the hard gate obol
gives on self-managed Slurm. That caveat must be stated honestly to any PCS operator.

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

### Unknown / to confirm ❓
1. **Enforcement integrity** (above) — can the client-side gate be bypassed, and does
   pairing with an allowlisted hard limit (QOS/association/TRES) close it? This is the
   feasibility-defining unknown.
2. **`obold` off-controller transport** — obol's Lua transport is **local-UDS-only**
   today; a login-node `obold` reached from compute-node prolog/epilog needs a network
   transport. Scope of that change.
3. **`admin_comment` writability from `cli_filter`** on PCS Slurm 25.11 for the
   correlation token, or a fallback carrier.
4. **SETTLE via Epilog vs. slurmdbd feed** — which is robust on PCS given no jobcomp
   script; the slurmdbd feed (#13) may be the better PCS-specific path.
5. **Where obold's state lives** and single-writer fencing on a login node
   (mirrors the ParallelCluster durability unknowns).
6. **PCS Slurm version** — docs reference 24.11 / 25.05 / 25.11; obol is tested on
   22.05 / 23.11 / 24.05. The 25.x line is **outside obol's current tested set** and
   needs a compatibility pass (the multi-gen tier extended to 25.x).

---

## Recommendation

PCS is **feasible but not free**. Sequence the investigation as:

1. **Resolve enforcement integrity first** (unknown #1). If the client-side
   `cli_filter` gate can be trivially bypassed and can't be backstopped by an
   allowlisted hard limit, PCS obol is a soft advisory tool, not a hard gate — decide
   whether that's acceptable *before* building anything.
2. **Prototype the `cli_filter/lua` gate** against a real PCS cluster (an
   `integ-pcs` tier analogous to `integ-pcluster`, honoring the same no-destructive
   rule — attach to an existing cluster, never `create-cluster`), proving GATE-reject
   and the token carrier.
3. **Add off-controller transport to the seam** (unknown #2) — the one clear code
   change obol needs regardless of the enforcement outcome.
4. **Re-home SETTLE** onto Epilog and/or the slurmdbd feed; validate on PCS Slurm
   25.x (unknowns #4, #6).

None of this touches the money kernel — the invariants
([`../CLAUDE.md`](../CLAUDE.md)) are unaffected; this is entirely about *where the
seam attaches* and *whether the attachment is a hard gate on a managed controller*.
Track it behind the ParallelCluster work, which is shippable now with no such
open questions.
