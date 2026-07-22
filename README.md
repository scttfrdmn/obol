<p align="center">
  <img src="docs/assets/obol-hero.png" alt="obol — fund the work before it runs. Monetary budget enforcement for Slurm." width="100%">
</p>

# obol

[![CI](https://github.com/scttfrdmn/obol/actions/workflows/ci.yml/badge.svg)](https://github.com/scttfrdmn/obol/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Obol gives Slurm administrators hard, money-denominated budgets for users, projects, and
accounts.** It reserves a job's estimated cost *at submission*, rejects jobs that can't be
funded, charges actual runtime when they finish, and refunds the unused reservation.

The point is the **gate at submit time**, not a report after the fact. Slurm's own accounting
(associations, QOS, fair-share, `sacct`, `TRESBillingWeights`) can *measure* and *rank* usage,
and cap it in service-unit terms — but it tells you what a job cost *after* the compute is
gone. Obol answers a different question *before* the job runs: **can this account afford it?**
On a cloud-backed or chargeback cluster, where compute is real money, that's the question that
matters. Obol runs alongside slurmdbd (it reuses Slurm's account tree; it does not replace
accounting) and enforces spend at the one place spend can still be prevented.

A worked example: a lab is funded **$10,000**. Alice submits a 4-node job estimated at **$320**.
Obol reserves $320 (available now $9,680) and admits it. The job finishes early, costing
**$241.60** — Obol books that and refunds **$78.40**. A job that would overrun the balance is
rejected at `sbatch` with a message, and never schedules.

Named for the [obol](https://en.wikipedia.org/wiki/Obol_(coin)), an ancient small-denomination coin.

## Maturity & compatibility

Pre-1.0 and released continuously (latest **v0.14.0**; see [`CHANGELOG.md`](CHANGELOG.md)). The
money kernel, daemon, wire protocol, and full CLI are built and tested; the Slurm seam is
validated end to end in a containerized single-node Slurm tier across all three target
generations.

| Piece | State |
|-------|-------|
| `internal/budget` — money kernel | **built & tested**: exact-integer conservation + concurrency under `-race`, crash-safe WAL + snapshot recovery |
| `obold` — sidecar daemon | **built & tested**: GATE/BIND/SETTLE, multi-account, burst dispatch, transfers, reconciliation, orphan janitors |
| `obol` — admin/diagnostic CLI | **built & tested**: account management, funding, pricing, simulation, burst dispatch, lifecycle ops, and reconciliation ([19 commands](docs/cli-reference.md)) |
| Slurm seam — `job_submit.lua` + prolog/jobcomp | **built & validated in Docker** on Slurm **22.05 / 23.11 / 24.05** (Rocky 8/9/10), from source ([`docs/INTEGRATION.md`](docs/INTEGRATION.md)) |
| `site_factor` burst-dispatch plugin | **reference C source** (`seam/plugin/`); the decision is daemon-side and tested, the plugin isn't yet CI-built |
| Production hardening | maturing — see [`docs/production-readiness.md`](docs/production-readiness.md) for validated environments, known limitations, and the go-live checklist |

**Compatibility (pre-1.0):** minor versions may break the wire protocol and on-disk state
format; patch versions are fixes. Slurm targets are 22.05 / 23.11 / 24.05. Not yet
recommended for unattended production without an operator familiar with the [seam
design](docs/SEAM_DESIGN.md). The architecture — sidecar daemon, three-tier latency model, the
`admin_comment` correlation token, owned-vs-rented partition policy — is in
[`docs/SEAM_DESIGN.md`](docs/SEAM_DESIGN.md).

## Quickstart

Obol is a sidecar daemon (`obold`) plus a Slurm `job_submit` shim that calls it. A minimal
multi-account deployment:

**1. Install the binaries on the controller** — grab a release from the
[releases page](https://github.com/scttfrdmn/obol/releases), or build them:

```
make build && sudo install bin/obold bin/obol /usr/local/bin/
```

**2. Write a config** (`obold.json`) — accounts with money balances, a cost rate, and a window:

```json
{
  "accounts": [
    {"name": "lab_smith", "balance": 100000, "rate": 1, "window": "720h"},
    {"name": "lab_jones", "balance": 50000,  "rate": 1, "window": "720h"}
  ]
}
```

`rate` is money per second of walltime (or set per-node/TRES pricing — see the config docs).

**3. Start the daemon** (defaults: socket `/run/obol/obold.sock`, state `/var/lib/obol`):

```
obold -config obold.json
```

**4. Install the shim** so slurmctld calls Obol at submit. In `slurm.conf`:

```
JobSubmitPlugins=lua      # with seam/lua/job_submit.lua installed in the plugin dir
Prolog=/usr/local/bin/obol-prolog.sh
JobCompType=jobcomp/script
JobCompLoc=/usr/local/bin/obol-jobcomp.sh
```

See [`docs/INTEGRATION.md`](docs/INTEGRATION.md) and [`seam/README.md`](seam/README.md) for the
exact install (the containerized tier in `test/docker/` is a complete working example).

**5. Submit — funded runs, unfunded is rejected at `sbatch`:**

```
sbatch --account=lab_smith --time=60 job.sh     # reserved at submit; settled + refunded on exit
obol show --account lab_smith                    # balance, reserved, burn rate
```

`obol` talks to the default socket automatically; pass `--socket PATH` only if you
ran `obold` on a non-default socket.

<details>
<summary>Developer demo: drive the money lifecycle without Slurm</summary>

The gate/bind/settle primitives the shim uses can be exercised directly against the daemon —
useful for testing the wire protocol, not how an operator uses Obol. This one uses a
throwaway socket under `/tmp`, so the commands pass `--socket` to match:

```
obold -socket /tmp/obold.sock -state-dir /tmp/obol -create -balance 5000 -rate 1 &
obol --socket /tmp/obold.sock show
tok=$(obol --socket /tmp/obold.sock gate --account lab --partition cloud --time-limit 1000)
obol --socket /tmp/obold.sock bind   --token "${tok#allow }" --jobid 42
obol --socket /tmp/obold.sock settle --jobid 42 --kind complete --runtime 300
obol --socket /tmp/obold.sock show   # balance debited by the 300s consumed, tail refunded
```
</details>

## Documentation

**[`docs/`](docs/) is the documentation map** — it routes you by intent (evaluating
/ trying / deploying / operating / contributing). The key pages:

- **[`docs/concepts.md`](docs/concepts.md)** — how Obol works, built up from one
  worked example: reservations, settlement, hierarchy, windows, banked burst,
  costing, why money is integer, and **how Obol relates to Slurm
  accounting/QOS/fair-share**. Start here.
- **[`docs/installation.md`](docs/installation.md)** — get the binaries, run the
  daemon, service unit, socket/permissions, verify. Deploy here.
- **[`docs/configuration.md`](docs/configuration.md)** — the full `obold.json`
  reference: accounts, rates, windows, burst, node-type pricing, admins, and the
  runtime-change verbs.
- **[`docs/operations.md`](docs/operations.md)** — running it safely: durability &
  backup/restore, recovery after failure, the orphan janitors + `reconcile`,
  fail-open/closed, monitoring, upgrades/rollback, and the admin model.
- **[`docs/cli-reference.md`](docs/cli-reference.md)** — every `obol` verb, its
  flags, and exit codes.
- **[`docs/production-readiness.md`](docs/production-readiness.md)** — compatibility
  matrix, CI-tested vs. locally-validated tiers, lifecycle diagrams, and a go-live
  checklist.
- **[`docs/SEAM_DESIGN.md`](docs/SEAM_DESIGN.md)** — the architecture and *why* it
  looks that way (sidecar daemon, three-tier latency, correlation token, failure modes).
- **[`docs/INTEGRATION.md`](docs/INTEGRATION.md)** — the test tiers and what each proves
  (unit, Docker Slurm, multi-generation, ParallelCluster).

## Build

```
make build     # -> bin/obold
make check     # fmt + vet + lint + race  (what CI enforces)
```

Requires Go 1.26 (single supported toolchain; CI runs 1.26 only).

## Design invariants

The kernel holds these exactly, and they must never regress (see `CLAUDE.md`):

- **Conservation:** `B0 == B + reserved + consumed + write_off`, with integer money.
- **Atomic gate:** check-and-debit as one locked operation — no overdraft race.
- **Deterministic replay:** transitions are pure functions of `(state, command, now)`; the WAL
  logs commands and replays them through the same code paths used live.

## Contributing

GitHub is the source of truth for planning (issues/milestones/labels). PRs only; `main` is
protected. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

Apache-2.0. See [`LICENSE`](LICENSE).
