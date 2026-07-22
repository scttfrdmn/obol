# Integration testing

obol has four test tiers, in increasing fidelity and cost:

| Tier | Command | What it proves | Needs |
|------|---------|----------------|-------|
| **Unit** | `make test` / `make race` | kernel invariants, wire framing, daemon/CLI over a socket, Lua↔Go framing | Go (+ `lua` for the seam cross-check) |
| **Docker Slurm** | `make integ-docker` | the **real** GATE seam against an actual `slurmctld` (packaged 22.05): gate → escrow → run → jobcomp SETTLE → refund, plus multi-user/multi-account submission | Docker |
| **Docker multi-gen** | `make integ-docker-multigen` | the seam against Slurm **built from source** at each burstlab generation's exact version (22.05 / 23.11 / 24.05) — resolves the per-generation §10 ABI question | Docker (slow: ~10-20 min/image) |
| **ParallelCluster** | `make integ-pcluster` | the seam on real multi-node AWS Slurm with cloud partition policy | an AWS PC cluster + creds |

The default `go test ./...` runs only the unit tier; the integration tiers are
behind build tags (`docker_integration`, `docker_multigen`, `integration`) and
skip cleanly when their environment is absent.

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
stamps and reads the token on 22.05. Newer generations (23.11 / 24.05) are the
multi-gen tier below.

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

## Docker multi-generation Slurm (`make integ-docker-multigen`)

The packaged tier proves the seam on one Slurm version (22.05). burstlab spans
three Slurm generations, and `SEAM_DESIGN.md` §10 notes the Lua `job_desc` field
set (`admin_comment` read/write, `tres`/`time_limit`, `site_factor`) can vary
between them — so each generation must be validated. 23.11 and 24.05 are **not
packaged** (EPEL has only 22.05 on Rocky 9), so this tier builds Slurm **from the
SchedMD source tarball**, one image per generation, matching burstlab's packer
AMIs (`~/src/burstlab/ami/*.pkr.hcl`):

| Gen | Base | Slurm | Matches burstlab |
|-----|------|-------|------------------|
| gen1 | Rocky 8 | 22.05.11 | `gen1-slurm2205-rocky8` |
| gen2 | Rocky 9 | 23.11.10 | `gen2-slurm2311-rocky9` |
| gen3 | Rocky 10 | 24.05.5 | `gen3-slurm2405-rocky10` |

```
make integ-docker-multigen                     # all defined generations
make integ-docker-multigen OBOL_INTEG_GENS=gen2   # one generation
```

`test/docker/Dockerfile.slurm-src` is parameterized by `BASE_IMAGE` /
`SLURM_VERSION` / `ENABLE_CRB`; the harness (`test/docker/multigen_test.go`,
`//go:build docker_multigen`) builds + boots + exercises each generation in its
own image/container. Each generation runs the funded lifecycle and asserts the
**admin_comment token round-trip** — the §10 concern — proving the shim's
`slurm_job_submit` write works on that Slurm version.

**Deliberate divergences from burstlab's AMIs** (a self-contained container, not
an EFS-backed cloud node):
- **Prefix `/usr`, not `/opt/slurm-baked`** — burstlab uses `/opt` for its
  EFS-rsync pattern; a container needs Slurm on the default path so the shared
  `entrypoint.sh` (`/usr/sbin/slurmctld` …) works unchanged across both images.
- **No `--enable-slurmrestd`** — the seam doesn't use the REST API, and EPEL 10
  dropped `http-parser-devel`; omitting it keeps one recipe for all three gens.
- **`lua-devel` added** to the build deps (beyond burstlab's AMI set) — the GATE
  seam is a `JobSubmitPlugins=lua` plugin, so `configure` must build
  `job_submit_lua.so` (filed as burstlab#6 for its optional-obol path).
- **`task/none` + `proctrack/linuxproc`** kept from the packaged tier — this
  validates the obol **seam**, not Slurm's cgroup enforcement (which containers
  can't do cleanly); burstlab uses `task/cgroup` on real nodes. `dbus-devel` +
  `kernel-headers` are still installed so `configure` builds the cgroup/v2 plugin
  (matching burstlab), even though the tests don't rely on cgroup controllers.

**Base-OS differences the shared `entrypoint.sh` handles** (found bringing up all
three): the munge key tool is `create-munge-key` on Rocky 8/9 but `mungekey` on
Rocky 10; the MariaDB daemon is `/usr/libexec/mysqld` + `mysql_install_db` on
Rocky 8 (MariaDB 10.3) but `/usr/libexec/mariadbd` + `mariadb-install-db` on Rocky
9/10. The entrypoint detects both so one script boots every generation.

## Scope of the seam under test

The Docker tier drives settlement through the **controller-side `jobcomp/script`
feed** (#13) with no epilog installed, so it proves that path independently.

This exercises the **GATE + SETTLE** money path only. It does **not**
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
