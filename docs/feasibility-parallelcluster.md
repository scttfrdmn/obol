# Feasibility: obol on AWS ParallelCluster

**Status:** feasible today — this is a *how*, not an *if*.
**Scope:** design/feasibility only; no deploy tooling ships in this doc.
**Sources:** obol's own [`SEAM_DESIGN.md`](SEAM_DESIGN.md), the existing
ParallelCluster integration harness (`test/integration/pcluster_test.go`), and the
current (July 2026) AWS ParallelCluster documentation.

---

## TL;DR

AWS ParallelCluster gives you a **head node you own** — a normal EC2 instance in
your account, with root, running a normal `slurmctld` you configure through
`slurm.conf`. That is exactly the environment obol's seam was designed for. Every
attachment point obol needs — `JobSubmitPlugins=lua` for the GATE, `Prolog` for the
BIND, `JobCompType=jobcomp/script` for the SETTLE, a local Unix socket to `obold` —
is available with no compromise. **obol runs on ParallelCluster the same way it runs
on any self-managed Slurm; there is no seam redesign.** The only ParallelCluster-
specific work is *packaging*: getting the binaries and seam onto the head node and
surviving head-node replacement.

The integration harness already proves this end to end against a real cluster
(`make integ-pcluster`), so the confirmed/unknown split below leans heavily toward
confirmed.

---

## Why ParallelCluster is the easy case

ParallelCluster provisions the cluster for you, but it does **not** take the head
node away from you the way a fully managed service does. Concretely:

| obol needs… | On ParallelCluster you have… | Confirmed? |
|-------------|------------------------------|:----------:|
| root/shell on the controller host | Yes — the head node is an EC2 instance you SSH into (default `ec2-user`/`ubuntu`/`rocky`, passwordless sudo) | ✅ |
| to run a long-lived `obold` next to slurmctld | Yes — install a systemd unit; nothing forbids extra daemons | ✅ |
| a local Unix socket the shim + CLI connect to | Yes — `/run/obol/obold.sock` on the head node, same as bare-metal | ✅ |
| `JobSubmitPlugins=lua` (the GATE) | Yes — set it in `slurm.conf`; PC lets you extend Slurm config | ✅ |
| `Prolog` + `PrologFlags=Alloc` (the BIND) | Yes | ✅ |
| `JobCompType=jobcomp/script` + `JobCompLoc` (the SETTLE) | Yes | ✅ |
| to persist money state on durable storage | Yes — head-node EBS, or better, a shared filesystem (see below) | ✅ |

Because all of these are present, **the seam is unchanged from the self-managed
install** documented in [`installation.md`](installation.md) and
[`INTEGRATION.md`](INTEGRATION.md). obol does not need to know it's running on
ParallelCluster.

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

Three viable mechanisms, in increasing "productionness":

1. **Post-install / `OnNodeConfigured` custom action** (recommended for a
   reproducible cluster). ParallelCluster runs a script you point it at (an S3 URL)
   on the head node after configuration. Have it: pull the `obold`/`obol` release
   binaries, install the Lua seam + prolog/jobcomp, drop the systemd unit, and append
   the four `slurm.conf` lines. This bakes obol into `pcluster create-cluster` /
   `update-cluster` so a rebuilt head node comes back with obol already wired.
2. **Custom AMI.** Bake the binaries + seam into an AMI and point the cluster's head
   node at it; the post-install action then only writes config. Fastest boot,
   heaviest to maintain.
3. **SSH push (what the test harness does).** `scp` the binaries + seam to a running
   head node, install, `scontrol reconfigure`. Great for iteration and exactly how
   `test/integration/pcluster_test.go` validates the seam — **not** durable across
   head-node replacement on its own, so pair it with (1) for anything real.

The seam files and their placement are already documented in
[`../seam/README.md`](../seam/README.md); the ParallelCluster wrapper is just "run the
existing install steps via a custom action instead of by hand."

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
- Head node is a customer-owned EC2 instance with root/shell and a configurable
  `slurm.conf`.
- All four seam hooks (`JobSubmitPlugins`, `Prolog`/`PrologFlags`, `JobCompType`/
  `JobCompLoc`) are settable.
- The GATE → BIND → SETTLE lifecycle works end to end on real multi-node PC Slurm
  (integration harness).
- `admin_comment` is writable on the target Slurm generations (also confirmed in the
  Docker multi-gen tier: 22.05 / 23.11 / 24.05).
- ParallelCluster supports custom bootstrap actions and shared storage — the two
  primitives the packaging needs.

### Unknown / to confirm ❓
1. **Head-node replacement fencing.** Confirm that ParallelCluster never runs two
   head nodes concurrently against the same shared state dir, and that a replacement
   head node's `obold` starts cleanly on the recovered WAL. (Design intent: single
   writer; recovery is idempotent — needs a real replacement-drill to prove.)
2. **`update-cluster` behavior on the seam.** Confirm a `pcluster update-cluster`
   that rebuilds the head node re-runs the custom action and re-wires obol (vs.
   silently dropping the `slurm.conf` additions). Whether PC's config management
   preserves or overwrites manual `slurm.conf` edits is the crux — favor the custom
   action over hand-editing for exactly this reason.
3. **State-dir on FSx/EFS durability semantics.** Confirm `fdatasync` on the chosen
   shared FS actually flushes to durable storage (EFS/Lustre POSIX semantics) so the
   WAL's crash-safety guarantee ([`operations.md`](operations.md#durability-model-whats-on-disk-and-why-a-crash-is-safe))
   holds. Lustre and EFS both claim POSIX fsync; verify under `-sync true`.
4. **Multi-user identity on the head node.** obol's admin gate uses `SO_PEERCRED`
   (uid/gid of the socket peer). Confirm the PC head node's user model (who submits,
   who administers) maps cleanly to obol's `admin_users`/`admin_groups`
   ([`operations.md`](operations.md#who-may-administer)).
5. **Slurm version PC ships.** Confirm the PC version's Slurm is within obol's tested
   set (22.05 / 23.11 / 24.05) or run the multi-gen tier against whatever PC ships.
6. **Burst dispatch (`site_factor`).** The burst-headroom decision is daemon-side and
   tested, but the `site_factor` C plugin isn't CI-built or exercised on PC. If a
   cluster uses banked-burst dispatch, that plugin path needs a PC validation.

None of these are feasibility *blockers* — they're packaging/hardening items. The
architecture question ("can obol attach to ParallelCluster at all?") is answered:
**yes, with no seam changes.**

---

## Recommendation

Ship a **ParallelCluster bootstrap bundle**: a `custom action` script (S3-hosted)
that installs the release binaries + seam and wires the four `slurm.conf` lines, plus
guidance to put `-state-dir` on managed shared storage. That turns the
already-proven SSH install into a reproducible `pcluster create-cluster` artifact.
Track it as its own milestone; it depends on nothing in the kernel — only on
packaging and a real head-node-replacement drill to close unknowns (1) and (2).

Contrast with [`feasibility-pcs.md`](feasibility-pcs.md), where the managed
controller removes the shell/root and forbids `JobSubmitPlugins` — that is the
genuine feasibility question. ParallelCluster is not; it's a deployment exercise.
