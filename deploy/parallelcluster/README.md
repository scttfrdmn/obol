# obol on AWS ParallelCluster

A reproducible bootstrap bundle that installs the obol budget seam onto an AWS
ParallelCluster head node. It turns the by-hand SSH install (what the
`integ-pcluster` test does) into a `pcluster create-cluster` artifact so a rebuilt
head node comes back with obol already wired.

See [`docs/feasibility-parallelcluster.md`](../../docs/feasibility-parallelcluster.md)
for the design rationale and the confirmed-vs-unknown analysis.

## What's here

| File | Role |
|------|------|
| `install-obol.sh` | The installer. Runs as a ParallelCluster `OnNodeConfigured` custom action (or by hand as root). Installs the binaries + Lua seam + prolog/jobcomp, writes the obold systemd unit + config, starts obold, and `scontrol reconfigure`s. |
| `cluster.sample.yaml` | A ParallelCluster cluster config wired for obol: the `OnNodeConfigured` action and the `CustomSlurmSettings` seam directives. Replace the `PLACEHOLDER_*` values. |

## The attachment model (why it looks like this)

ParallelCluster gives you a **head node you own** (root, a real slurmctld), so all of
obol's seam mechanics work — but ParallelCluster **manages `slurm.conf`** and rewrites
it on every update. So the seam attaches through two supported extension points, not
by editing files slurmctld reads:

1. **`CustomSlurmSettings`** (cluster YAML) carries the four `slurm.conf` directives.
   ParallelCluster appends them and preserves them across updates (≥ 3.6.0).
2. **`OnNodeConfigured`** (custom action) puts the binaries/seam/daemon on the box.

### The seam directives

| obol step | Directive | Where it runs | ParallelCluster note |
|-----------|-----------|---------------|----------------------|
| **GATE** | `JobSubmitPlugins=lua` | slurmctld (head) | allowed |
| **BIND** | `PrologSlurmctld=…/obol-prolog-slurmctld.sh` | head node | `Prolog`/`Epilog` are **deny-listed**; `PrologSlurmctld` is not — so BIND runs controller-side |
| **SETTLE** | `JobCompType=jobcomp/script` + `JobCompLoc=…` | slurmctld (head) | allowed; fires even on node failure |

> **The one divergence from the generic seam:** BIND normally runs as `Prolog` on the
> compute node. ParallelCluster deny-lists `Prolog`/`Epilog` in `CustomSlurmSettings`
> because it uses them for its own node lifecycle. `PrologSlurmctld` runs on the head
> node — where `obold` and its socket already are — and is not deny-listed. The
> `bind` verb isn't admin-gated, so running it from slurmctld is fine. Tracked as a
> divergence in the feasibility doc.

## Deploy

1. **Stage the installer (and your budgets) in S3:**
   ```
   aws s3 cp deploy/parallelcluster/install-obol.sh s3://YOUR_BUCKET/obol/install-obol.sh
   aws s3 cp your-obold.json                        s3://YOUR_BUCKET/obol/obold.json
   ```
2. **Copy and fill in the cluster config:**
   ```
   cp deploy/parallelcluster/cluster.sample.yaml my-cluster.yaml
   # replace every PLACEHOLDER_* (region, subnet, key, bucket, obol release)
   ```
3. **Create the cluster:**
   ```
   pcluster create-cluster --cluster-name obol-demo --cluster-configuration my-cluster.yaml
   ```
4. **Verify** on the head node:
   ```
   ssh <head-node>
   obol list                      # accounts + balances from obold.json
   sbatch --account=<acct> --partition=cloud --time=1 --wrap='true'   # gated at submit
   ```

### By-hand install (existing cluster, no rebuild)

You can also run the installer over SSH on a running head node — useful for a trial
before committing it to the cluster YAML. This does **not** survive a head-node
replacement on its own (pair it with `CustomSlurmSettings` for that):

```
scp deploy/parallelcluster/install-obol.sh <head>:/tmp/
ssh <head> 'sudo OBOL_STATE_DIR=/shared/obol /tmp/install-obol.sh --release v0.14.0'
# then add the four CustomSlurmSettings directives (or, for a quick test only,
# append them to slurm.conf and `scontrol reconfigure` — lost on next update).
```

## State durability (read this)

`obold`'s state dir is **real money state** and the head node's root volume is
**ephemeral across head-node replacement**. Put `--state-dir` on the cluster's
**shared filesystem** (EFS/FSx, mounted via `SharedStorage` — `/shared/obol` in the
sample). A replacement head node recovers by replaying the WAL from that directory.

`obold` is a **single writer** — exactly one running `obold` per state dir. On a
single-head-node cluster that's automatic; the risk is only a replacement head node
starting while an old one still writes. See
[`docs/operations.md`](../../docs/operations.md#recovery-after-failure).

## Fail-open vs. fail-closed

ParallelCluster queues are cloud/rented compute, so they should **fail closed** — if
`obold` is unreachable at submit, reject rather than allow ungated spend. Set the
per-partition `fail_closed` table in `seam/lua/job_submit.lua` to match your queue
names before staging the release (or bake a patched seam via `--source`). See
[`docs/operations.md`](../../docs/operations.md#fail-open-vs-fail-closed-what-happens-if-obold-is-down).

## Open items

The feasibility doc tracks what a production rollout still needs to prove: head-node-
replacement fencing (single writer), that `update-cluster` re-runs the action and
preserves `CustomSlurmSettings`, EFS/FSx `fdatasync` durability under `-sync true`,
and the burst `site_factor` plugin on PC. None are blockers for a GATE+SETTLE
deployment.
