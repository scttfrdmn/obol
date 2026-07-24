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
| `install-obol.sh` | The installer, **phase-aware**: `--phase files` lays down the binaries + Lua seam + prolog/jobcomp; `--phase daemon` writes the obold config + systemd unit, starts obold, and `scontrol reconfigure`s; `--phase all` does both (by-hand/custom-AMI installs). |
| `cluster.sample.yaml` | A ParallelCluster cluster config wired for obol: the two custom actions (`OnNodeStart`→files, `OnNodeConfigured`→daemon) and the `CustomSlurmSettings` seam directives. Replace the `PLACEHOLDER_*` values. |

> **Ordering matters (learned from a real cluster failure).** ParallelCluster starts
> `slurmctld` during its own bootstrap, **before** `OnNodeConfigured` runs. With
> `JobSubmitPlugins=lua` set, slurmctld fatals at startup if `job_submit.lua` isn't
> already on disk — failing the whole head-node bootstrap. So the seam **files** are
> installed at **`OnNodeStart`** (`--phase files`), and the **daemon** is started at
> `OnNodeConfigured` (`--phase daemon`). PC loads the plugin from `/opt/slurm/etc/`.

## The attachment model (why it looks like this)

ParallelCluster gives you a **head node you own** (root, a real slurmctld), so all of
obol's seam mechanics work — but ParallelCluster **manages `slurm.conf`** and rewrites
it on every update. So the seam attaches through two supported extension points, not
by editing files slurmctld reads:

1. **`CustomSlurmSettings`** (cluster YAML) carries the four `slurm.conf` directives.
   ParallelCluster appends them and preserves them across updates (≥ 3.6.0).
2. **Two custom actions** put the seam on the box in the right order:
   `OnNodeStart` runs `install-obol.sh --phase files` (before slurmctld), and
   `OnNodeConfigured` runs `--phase daemon` (starts obold after shared storage mounts).

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

## Verified on a live cluster

The full lifecycle was run end to end on a real ParallelCluster (PC 3.15.1,
`alinux2023`, **Slurm 25.11**), and it works:

- **GATE** — a funded `sbatch` escrowed its projected cost (60 units for a 60 s job),
  balance debited, token stamped into `admin_comment`.
- **BIND** — `PrologSlurmctld` bound the token to the job id at allocation (a `start`
  ledger entry).
- **SETTLE** — `jobcomp/script` billed the actual 21 s runtime and refunded the tail;
  **conservation held** (`sum 100000, B0 100000`).
- **Reject** — an unfundable job was rejected at submit (`obol: job rejected —
  insufficient budget`), nothing escrowed.

Getting there surfaced four things a stock PC AMI needs, all now handled by
`install-obol.sh` (or flagged below):

1. **Files before slurmctld** — the `OnNodeStart`/`OnNodeConfigured` phase split
   (see the ordering note above). Without it the head-node bootstrap fails.
2. **A Lua socket backend** — stock PC (Slurm 25.11's embedded Lua 5.4) ships no
   luasocket, no LuaJIT FFI, and no `lua-socket` package / EPEL / luarocks. The GATE
   shim falls back to exec'ing `obol gate` in that case (#137), so it still enforces
   without a backend — but that forks per submit, so `install-obol.sh` also builds
   luasocket (incl. the AF_UNIX submodule) from source using the AMI's `gcc` +
   `lua-devel`, into `/usr/lib64/lua/<ver>/socket/`, as the fast in-process path.
   (If the build fails, the shell-out fallback keeps the gate working.)
3. **`scontrol` on the prolog's PATH** — slurmctld runs `PrologSlurmctld` with a
   minimal env; `obol-prolog.sh` needs `scontrol` to read the token from
   `admin_comment`, or BIND silently no-ops. The generated wrapper adds the Slurm
   `bin` dir to PATH.
4. **Socket group for the `slurm` user** — slurmctld runs as `slurm`; connecting to
   the root-owned socket needs it group-writable. `obold -socket-group slurm
   -socket-mode 0660` (#136) sets this at listen time; the installer passes those
   flags when the installed obold supports them, and falls back to an ExecStartPost
   `chgrp`/`chmod` on older builds.

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

Beyond the four handled above, a production rollout should still prove: head-node-
replacement fencing (single writer), that `update-cluster` re-runs the action and
preserves `CustomSlurmSettings`, EFS/FSx `fdatasync` durability under `-sync true`,
the burst `site_factor` plugin on PC, and obol's seam against **Slurm 25.11**
(outside the currently tested 22.05/23.11/24.05 set — it worked in this run, but
isn't in CI). Of the four fixes above, the socket-group one is now a proper
**daemon feature** (`obold -socket-group`/`-socket-mode`, #136); a robust Lua
transport backend (#137) is still tracked as a follow-up.
