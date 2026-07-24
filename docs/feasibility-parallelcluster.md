# Feasibility: obol on AWS ParallelCluster

**Status:** feasible today — this is a *how*, not an *if*.
**Scope:** design/feasibility only. A working deploy bundle lives in
[`deploy/parallelcluster/`](../deploy/parallelcluster/).
**Sources:** obol's own [`SEAM_DESIGN.md`](SEAM_DESIGN.md), the existing
ParallelCluster integration harness (`test/integration/pcluster_test.go`), and the
current (July 2026) AWS ParallelCluster documentation — specifically the
`CustomSlurmSettings` deny-list and the custom-bootstrap-actions pages.

---

## TL;DR

AWS ParallelCluster gives you a **head node you own** — a normal EC2 instance in
your account, with root, running a normal `slurmctld`. That is essentially the
environment obol's seam was designed for, so obol runs on ParallelCluster with **one
small seam divergence and no kernel changes**.

The one wrinkle: ParallelCluster **manages `slurm.conf` itself** and rewrites it on
every update, so you don't hand-edit it — you add Slurm directives through the
supported **`CustomSlurmSettings`** field in the cluster YAML, which enforces a
**deny-list**. Three of obol's four seam directives pass the deny-list unchanged
(`JobSubmitPlugins`, `JobCompType`, `JobCompLoc`); the fourth — `Prolog`, obol's BIND
— is **deny-listed** (PC uses `Prolog`/`Epilog` for its own node lifecycle). The fix
is clean: move BIND to **`PrologSlurmctld`**, which is *not* deny-listed and runs on
the head node where `obold` already lives. That's the whole divergence.

So the ParallelCluster-specific work is *packaging* (getting the binaries + seam onto
the head node via a bootstrap action, carrying the directives via `CustomSlurmSettings`,
and surviving head-node replacement) plus that one BIND re-home. The integration
harness proves the GATE→BIND→SETTLE mechanics end to end against a real cluster
(`make integ-pcluster`), so the confirmed/unknown split below leans toward confirmed.

---

## Why ParallelCluster is the easy case

ParallelCluster provisions the cluster for you, but it does **not** take the head
node away from you the way a fully managed service does. Concretely:

| obol needs… | On ParallelCluster you have… | Confirmed? |
|-------------|------------------------------|:----------:|
| root/shell on the controller host | Yes — the head node is an EC2 instance you SSH into (default `ec2-user`/`ubuntu`/`rocky`, passwordless sudo) | ✅ |
| to run a long-lived `obold` next to slurmctld | Yes — install a systemd unit; nothing forbids extra daemons | ✅ |
| a local Unix socket the shim + CLI connect to | Yes — `/run/obol/obold.sock` on the head node, same as bare-metal | ✅ |
| `JobSubmitPlugins=lua` (the GATE) | Yes — via `CustomSlurmSettings`; **not** deny-listed | ✅ |
| a start-time BIND hook | Yes — but as **`PrologSlurmctld`**, not `Prolog`: `Prolog`/`Epilog` are **deny-listed** by `CustomSlurmSettings` | ✅ (re-homed) |
| `JobCompType=jobcomp/script` + `JobCompLoc` (the SETTLE) | Yes — via `CustomSlurmSettings`; **not** deny-listed | ✅ |
| to persist money state on durable storage | Yes — head-node EBS, or better, a shared filesystem (see below) | ✅ |

### How Slurm config gets in: `CustomSlurmSettings`, not a hand-edit

ParallelCluster (≥ 3.6.0) **owns `slurm.conf`** and rewrites it on every
`update-cluster`, so a manual edit is silently lost. The supported, update-safe path
is the **`CustomSlurmSettings`** field in the cluster YAML
(`Scheduling.SlurmSettings.CustomSlurmSettings`), which appends parameters to
`slurm.conf` and preserves them across updates. It enforces a **deny-list** of
parameters that conflict with ParallelCluster's own logic. From the current
(July 2026) deny-list:

- **`Prolog` and `Epilog` are deny-listed** — ParallelCluster uses them for its node
  lifecycle. This is the one collision with obol's generic seam.
- `JobSubmitPlugins`, `JobCompType`, `JobCompLoc`, and `PrologSlurmctld` are **not**
  deny-listed, so obol's GATE, SETTLE, and (re-homed) BIND all attach cleanly.

### The one divergence: BIND on `PrologSlurmctld`

obol's generic seam binds token↔jobid in a **`Prolog`** on the compute node. Since
`Prolog` is deny-listed, on ParallelCluster the BIND moves to **`PrologSlurmctld`**,
which runs on the **head node** (the controller) — where `obold` and its socket
already are. The existing `obol-prolog.sh` works unchanged there: it reads the job's
`admin_comment` token and node features via `scontrol` and calls `obol bind` over the
local socket (and `bind` isn't an admin-gated verb, so running it from slurmctld is
fine). A generated wrapper sets `OBOL_BIN`/`OBOL_SOCKET` because slurmctld runs
prologs with a minimal environment. This is the sole seam change; the money kernel and
wire protocol are untouched. Tracked in [`divergence`](#divergence-from-the-generic-seam)
below.

---

## Recommended attachment model

### 1. Where obold runs

On the **head node**, as a systemd unit, started `Before=slurmctld` — identical to
the bare-metal guidance in [`installation.md`](installation.md#4-run-it-as-a-service).
The `job_submit` plugin calls it over the local Unix socket on the same host, so the
gate stays sub-millisecond on the controller lock.

### 2. Where the money state lives (the one real design decision)

The head node's root EBS volume is **ephemeral across head-node replacement** — if
ParallelCluster (or you) replaces the head node, a state dir on the root volume is
gone. obol's state dir is real money state, so:

- **Recommended:** put `-state-dir` on a **shared, durable filesystem** attached to
  the head node — an **FSx for Lustre** or **EFS** mount that ParallelCluster manages
  as `SharedStorage`. It survives head-node replacement, and a replacement head node
  recovers by replaying the WAL from the same directory (obol's recovery is
  idempotent and safe — [`operations.md`](operations.md#recovery-after-failure)).
- **Caveat to confirm (see unknowns):** `obold` is a **single writer**. The state dir
  must be mounted to *exactly one* running `obold` at a time. This is not a
  clustered/shared-writer store. On a single-head-node cluster that's automatic; the
  risk is only if a replacement head node's `obold` starts while an old one is still
  writing. Fence on that (systemd ordering + the fact PC replaces, not duplicates,
  the head node).
- **Simplest (dev/eval):** root EBS. Fine for a feasibility trial; **back it up** and
  understand you lose state if the head node is replaced.

> A shared filesystem is *storage*, not *coordination*. obol is still one process
> writing one directory; the shared FS just makes that directory outlive the instance.

### 3. Installing the seam onto the head node

The seam splits into **two** ParallelCluster extension points — the *files/daemon* go
on via a bootstrap action, and the *slurm.conf directives* go in via
`CustomSlurmSettings`:

**Files + daemon → `OnNodeConfigured` custom action.** ParallelCluster runs a script
you point it at (an S3 URL) on the head node after configuration. It pulls the
`obold`/`obol` release binaries, installs the Lua seam + prolog/jobcomp + the
`PrologSlurmctld` wrapper, drops the systemd unit, starts `obold`, and
`scontrol reconfigure`s. This is [`deploy/parallelcluster/install-obol.sh`](../deploy/parallelcluster/install-obol.sh).

**Directives → `CustomSlurmSettings`** (cluster YAML), *not* a `slurm.conf` edit
(which PC overwrites on update):
```yaml
Scheduling:
  SlurmSettings:
    CustomSlurmSettings:
      - JobSubmitPlugins: lua
      - PrologSlurmctld: /usr/local/bin/obol-prolog-slurmctld.sh
      - JobCompType: jobcomp/script
      - JobCompLoc: /usr/local/bin/obol-jobcomp.sh
```

Alternatives: bake binaries + seam into a **custom AMI** (fastest boot, heaviest to
maintain), or the **SSH push** the test harness uses (`test/integration/pcluster_test.go`
— great for iteration, but not durable across head-node replacement on its own, and it
hand-edits `slurm.conf` because it never calls `update-cluster`). The full bundle and
a sample cluster YAML are in [`deploy/parallelcluster/`](../deploy/parallelcluster/).

### 4. Fail-open vs. fail-closed on cloud partitions

ParallelCluster partitions are almost always **cloud/rented** EC2 (on-demand or
spot), so they should fail **closed** — if `obold` is unreachable at submit, reject
rather than let ungated spend onto real-money compute. Set the per-partition
`fail_closed` table in `job_submit.lua` accordingly
([`operations.md`](operations.md#fail-open-vs-fail-closed-what-happens-if-obold-is-down)).
This is the whole reason the fail-mode policy is per-partition: a PC cluster's queues
are the fail-closed case by default.

---

## What's already proven (the integration tier)

`make integ-pcluster` (`test/integration/pcluster_test.go`, `//go:build integration`)
deploys the seam to a **real, already-running** PC head node over SSH and asserts:

- a funded job **gates** (escrow taken), carries the correlation token in
  `admin_comment`, **settles** via the epilog on completion, bills only actual
  runtime, refunds the tail, and **conservation holds**;
- an unfunded job is **rejected at `sbatch`** with nothing escrowed.

It **never creates or destroys AWS resources** — the cluster must already exist
(CLAUDE.md's no-destructive rule; there is no `pcluster create-cluster` in the
harness). It exercises GATE + epilog-SETTLE; it does **not** yet exercise the burst
`site_factor` plugin or a slurmdbd completion feed.

So the *seam mechanics* on ParallelCluster are confirmed. What's below is what a
*production packaging* of that would still need to nail down.

---

## Confirmed vs. unknown

### Confirmed ✅
- Head node is a customer-owned EC2 instance with root/shell.
- Slurm config is set via **`CustomSlurmSettings`** (PC ≥ 3.6.0), which appends to
  `slurm.conf` and survives updates. `Prolog`/`Epilog` are **deny-listed**;
  `JobSubmitPlugins`, `JobCompType`, `JobCompLoc`, and `PrologSlurmctld` are **not**.
- obol's four seam directives all attach: GATE (`JobSubmitPlugins`), BIND re-homed to
  `PrologSlurmctld`, SETTLE (`JobCompType`/`JobCompLoc`).
- The GATE → BIND → SETTLE lifecycle works end to end on real multi-node PC Slurm
  (integration harness).
- `admin_comment` is writable on the target Slurm generations (also confirmed in the
  Docker multi-gen tier: 22.05 / 23.11 / 24.05).
- ParallelCluster supports custom bootstrap actions (`OnNodeConfigured`) and shared
  storage (`SharedStorage`) — the two primitives the packaging needs.

### Unknown / to confirm ❓
1. **Head-node replacement fencing.** Confirm that ParallelCluster never runs two
   head nodes concurrently against the same shared state dir, and that a replacement
   head node's `obold` starts cleanly on the recovered WAL. (Design intent: single
   writer; recovery is idempotent — needs a real replacement-drill to prove.)
2. **`update-cluster` re-runs the action + preserves `CustomSlurmSettings`.** Confirm
   a `pcluster update-cluster` (and a head-node rebuild) re-runs `OnNodeConfigured`
   and keeps the seam directives — the reason obol goes through `CustomSlurmSettings`
   rather than a `slurm.conf` edit (which PC *does* overwrite).
3. **`PrologSlurmctld` env + node-type true-up.** `PrologSlurmctld` runs on the head
   node, not the compute node, so the `#65` node-type reprice reads the assigned
   node's features via `scontrol` rather than local env. Confirm the reprice still
   resolves the node type correctly from the controller side.
4. **State-dir on FSx/EFS durability semantics.** Confirm `fdatasync` on the chosen
   shared FS actually flushes to durable storage (EFS/Lustre POSIX semantics) so the
   WAL's crash-safety guarantee ([`operations.md`](operations.md#durability-model-whats-on-disk-and-why-a-crash-is-safe))
   holds. Lustre and EFS both claim POSIX fsync; verify under `-sync true`.
5. **Multi-user identity on the head node.** obol's admin gate uses `SO_PEERCRED`
   (uid/gid of the socket peer). Confirm the PC head node's user model (who submits,
   who administers) maps cleanly to obol's `admin_users`/`admin_groups`
   ([`operations.md`](operations.md#who-may-administer)).
6. **Slurm version PC ships.** Confirm the PC version's Slurm is within obol's tested
   set (22.05 / 23.11 / 24.05) or run the multi-gen tier against whatever PC ships.
7. **Burst dispatch (`site_factor`).** The burst-headroom decision is daemon-side and
   tested, but the `site_factor` C plugin isn't CI-built or exercised on PC. If a
   cluster uses banked-burst dispatch, that plugin path needs a PC validation.

None of these are feasibility *blockers* — they're packaging/hardening items. The
architecture question ("can obol attach to ParallelCluster at all?") is answered:
**yes, with one small seam divergence (BIND on `PrologSlurmctld`) and no kernel
changes.**

---

## Divergence from the generic seam

On ParallelCluster, obol's **BIND step runs as `PrologSlurmctld` (head node) instead
of `Prolog` (compute node)**,
because ParallelCluster deny-lists `Prolog`/`Epilog` in `CustomSlurmSettings`. The
`obol-prolog.sh` logic is unchanged — only the Slurm directive it's installed under,
and a small wrapper that injects `OBOL_BIN`/`OBOL_SOCKET` into slurmctld's minimal
environment. No wire-protocol or kernel change. SETTLE stays on `jobcomp/script`
(controller-side, allowed), so the deny-listed `Epilog` fallback simply isn't used on
PC.

---

## Recommendation

Ship the **ParallelCluster bootstrap bundle** in
[`deploy/parallelcluster/`](../deploy/parallelcluster/): `install-obol.sh` (an
`OnNodeConfigured` action that installs the release binaries + seam and starts
`obold`), a sample `cluster.yaml` carrying the seam via `CustomSlurmSettings`, and
guidance to put `-state-dir` on managed shared storage. That turns the already-proven
SSH install into a reproducible `pcluster create-cluster` artifact. It depends on
nothing in the kernel — only on packaging, the one BIND re-home, and a real
head-node-replacement drill to close unknowns (1) and (2).

Contrast with [`feasibility-pcs.md`](feasibility-pcs.md), where the managed
controller removes the shell/root and forbids `JobSubmitPlugins` — that is the
genuine feasibility question. ParallelCluster is not; it's a deployment exercise.
