# Integration testing

obol has three test tiers, in increasing fidelity and cost:

| Tier | Command | What it proves | Needs |
|------|---------|----------------|-------|
| **Unit** | `make test` / `make race` | kernel invariants, wire framing, daemon/CLI over a socket, Lua↔Go framing | Go (+ `lua` for the seam cross-check) |
| **Docker Slurm** | `make integ-docker` | the **real** GATE seam against an actual `slurmctld`: gate → escrow → run → epilog SETTLE → refund, plus multi-user/multi-account submission | Docker |
| **ParallelCluster** | `make integ-pcluster` | the seam on real multi-node AWS Slurm with cloud partition policy | an AWS PC cluster + creds |

The default `go test ./...` runs only the unit tier; the integration tiers are
behind build tags (`docker_integration`, `integration`) and skip cleanly when
their environment is absent.

## Docker single-node Slurm (`make integ-docker`)

Builds `test/docker/Dockerfile.slurm` — a single Rocky 9 container running
munge + slurmctld + slurmd + slurmdbd + mariadb on one localhost node, with the
obol GATE seam installed (`job_submit.lua`, prolog/epilog) and the `obold`/`obol`
binaries baked in. The Go harness (`test/docker/slurm_test.go`) boots it,
submits jobs via `sbatch`, and asserts on the budget.

```
make integ-docker
```

**Slurm version.** EPEL ships Slurm **22.05** for Rocky 9, and that is the
deliberate target: 22.05 is burstlab Gen 1 and the generation whose
`admin_comment` writability was the first unconfirmed blocker
(`SEAM_DESIGN.md` §10 / §13 gap #1). This tier **confirms it works** — the gate
stamps and reads the token on 22.05. Newer generations (23.11 / 24.05) come from
source builds under issue #16.

**What the harness asserts:**
- `TestFundedJobLifecycle` — a funded job gates (60-unit escrow), runs, and the
  epilog settles it; only the actual runtime is billed and the tail refunded;
  conservation holds.
- `TestUnfundedJobRejected` — a job whose cost exceeds the balance is rejected at
  `sbatch` and nothing is escrowed.
- `TestMultiTenant` — jobs submitted as **four users across two Slurm accounts**
  (`alice`/`bob` in `lab_smith`, `carol`/`dave` in `lab_jones`); all gate, the
  concurrent-escrow low-water mark is observed, and conservation holds across the
  mix through settlement. (The obold MVP is single-budget; per-account budgets
  are the hierarchy work, #17/#18 — this fixture is what that work will split.)
- `TestGatedTokenStamped` — an admitted job carries `AdminComment=budget:<hex>`,
  proving `admin_comment` is writable on this generation.

**Container specifics worth knowing** (encoded in `entrypoint.sh` / `cgroup.conf`):
- Runs `--privileged` for a writable private cgroup namespace.
- `cgroup.conf` sets `IgnoreSystemd=yes`; the entrypoint pre-creates
  `/sys/fs/cgroup/system.slice/slurmstepd.scope` so slurmd (22.05, cgroup/v2)
  initializes without a system dbus.
- `slurm.conf` uses `proctrack/linuxproc` + `task/none` and
  `SlurmdParameters=config_overrides` so no real cgroup controllers are required.

## Scope of the seam under test

This exercises the **GATE + epilog-SETTLE** money path only. It does **not**
exercise the production burst dispatch gate (`site_factor`, v0.3.0) or the
slurmdbd completion feed (#13) — settlement here is driven by the epilog. That
is the honest boundary of what these tiers prove today.

## ParallelCluster (`make integ-pcluster`)

The cloud counterpart to the Docker tier: it deploys the seam to a **real,
already-running** AWS ParallelCluster head node over SSH, seeds a budget, and
drives the `sbatch` lifecycle on multi-node Slurm. The harness lives in
`test/integration/pcluster_test.go` (`//go:build integration`).

```
export OBOL_INTEG_CLUSTER=gauss              # enables the test
export OBOL_INTEG_HEAD=<head-node-ip>
export OBOL_INTEG_SSH_KEY=~/.ssh/cluster.pem
export OBOL_INTEG_SSH_USER=rocky             # optional (default rocky)
export OBOL_INTEG_PARTITION=serial-requeue   # optional
export OBOL_INTEG_ACCOUNT=obol_test          # optional
export OBOL_INTEG_ARCH=arm64                 # optional, for Graviton clusters
make integ-pcluster
```

With `OBOL_INTEG_CLUSTER` unset, the test **skips** — safe to run anywhere.

**What it does:** builds a linux `obold`/`obol`, `scp`s them plus the Lua seam
and prolog/epilog to the head node, installs them, seeds a fresh budget, wires
`JobSubmitPlugins=lua` + `Prolog`/`Epilog` into `slurm.conf`, and
`scontrol reconfigure`s. Then it submits a funded job (asserts escrow + token in
`admin_comment` + settle/refund + conservation) and an unfunded job (asserts
rejection). Teardown stops obold and removes the plugin lines.

**Safety / scope:**
- It **never creates or destroys AWS resources** — the cluster must already
  exist. There is no `pcluster create-cluster` here (CLAUDE.md's no-destructive
  rule). It only SSHes in and runs `sbatch`/`scontrol`/`sacctmgr`.
- It assumes passwordless `sudo` on the head node (the PC default admin user).
- It exercises **GATE + epilog-SETTLE** only — not the burst `site_factor` or the
  slurmdbd completion feed (#13).

**Partition → policy mapping.** Model the cloud/on-prem policy classes on the
sibling `gauss/` PC 3.15 project's partitions. In obol's fail-mode table
(`internal/shim`), a cloud/rented partition fails **closed** and an owned/on-prem
partition fails **open**. For a gauss-style cluster:

| gauss partition | class | obol fail mode |
|-----------------|-------|----------------|
| `serial-requeue`, `sapphire`, `bigmem` (on-demand/spot EC2) | cloud | fail closed |
| a reserved/owned queue, if any | on-prem | fail open |

Set `OBOL_INTEG_PARTITION` to the partition you want to exercise; the default
`serial-requeue` matches gauss.
