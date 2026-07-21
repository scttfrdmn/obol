# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Docker tier validates multi-account budgets on real Slurm (issue #18): obold boots with a
  two-account `-config` (`lab_smith` open, `lab_jones` restricted); tests prove per-account
  isolation (a `lab_smith` job debits only `lab_smith`, `lab_jones` untouched), access rejection
  (an unlisted user is rejected from a restricted account), and no-budget rejection (an
  unconfigured account is rejected) — alongside the existing lifecycle/token tests.
- Per-account budgets + resolution (issue #18): obold now holds a **registry of independent
  budgets, one per Slurm account** (`obold -config <json>`), each with its own WAL+snapshot dir.
  A submission's `--account` resolves to exactly that account's budget (exact match; no rollup —
  the #17 account-tree chain-debit was superseded by this flat model, with cross-budget spend
  tracked as a future feature #54). Unknown account → reject (SEAM §9). `obol show --account`
  selects one; `STATUS` gains an additive `Account` field. The kernel is **untouched** — each
  account conserves independently via the existing single-pot invariant.
- Optional per-account access enforcement: access defaults to **trusting Slurm** (slurmdbd already
  authorizes account membership); an account may set `allow_users`/`allow_groups` to further
  restrict, in which case obol resolves the submitter's uid→user/groups (via the OS, cached) and
  checks it — only incurred for restricted accounts, keeping the gate hot path lookup-free by
  default. `obold` without `-config` keeps the single-budget behavior unchanged.

## [0.2.0] - 2026-07-21

Gen 1 integration. The cost model now weights job cost by requested TRES, and a
controller-side jobcomp feed drives settlement reliably (even on node failure).
`admin_comment` writability on Slurm 22.05 — the last unconfirmed Gen 1 blocker —
is confirmed by the Docker tier. This is a minor bump: the wire protocol gains a
STATUS message and the kernel gains per-job-rate submit entry points, both
additive and backward-compatible.

### Added
- Controller-side completion feed (issue #13): a `jobcomp/script` hook
  (`seam/slurm/obol-jobcomp.sh`) that SETTLEs each job from slurmctld at completion, mapping Slurm
  state → complete/timeout/cancel/infrafail. This is the reliable settlement path — it fires even
  on node failure, unlike a compute-node epilog. The daemon needs no change (settle-by-jobid via
  the bind table). Adds `obol settle --if-present` so a completion hook that double-fires (jobcomp
  + epilog) is a benign no-op. Validated on real Slurm 22.05 with the epilog disabled (the Docker
  tier now settles purely via jobcomp). The epilog is retained as an optional redundant fallback.
- TRES-weighted cost model (issue #12): job cost can now depend on requested resources, not just a
  flat rate. The kernel gains a per-job rate — `Escrow.C` plus `SubmitAt`/`SubmitArrayAt(c, …)`
  (c≤0 falls back to the budget's flat `C`, so all existing behavior is unchanged); the rate is
  frozen per-escrow, logged in the WAL, and restored on recovery. The daemon maps requested TRES to
  a per-second rate via configured weights (`obold -tres-per-cpu|gpu|mem`, `daemon.Weights`); the
  Lua shim reads the TRES from `job_desc` and sends it on the GATE request. All-zero weights = flat
  rate (default). Validated end-to-end on real Slurm (a 2-CPU job billed 2× under a per-CPU weight);
  covered by kernel per-rate + mixed-rate + recovery tests and daemon weight tests. The
  dispatch-time true-up for cost-heterogeneous partitions is deferred to v0.3.0 (needs site_factor).

## [0.1.1] - 2026-07-20

Daemon-core hardening: the three deferred v0.1.0 items. Group-commit durability
takes the fsync off the caller's path, a lock-free tier-2 read path keeps
priority reads from contending the gate, and config durability is resolved as
immutable-after-creation. No wire-protocol or public-API changes.

### Added
- Tier-2 lock-cheap read path (issue #7): every mutation publishes an immutable
  `ReadView{B, burstPot, rLive}` under the lock it already holds; `ReadSnapshot()` loads it
  lock-free via an atomic pointer, so the frequent priority/burst reads never contend the gate
  write mutex. The triple is internally consistent (captured in one locked moment). Covered by a
  concurrent readers-vs-writers test under `-race`.
- WAL group commit (issue #6): `Append` writes the record to the page cache and returns; a
  background committer batches `fdatasync`s off the caller's path, so the fsync no longer serializes
  behind each append (the GATE ack returns after the in-memory escrow; durability lands a hair
  later). `Flush`/`Close` are synchronous durability barriers, and a failed background fsync is
  sticky and surfaced. The torn-tail discipline (invariant #4) and crash recovery are preserved —
  covered by concurrency (`-race`), recovery, and torn-tail tests.

### Changed
- Config durability (issue #8) resolved as **immutable-after-creation**: cost rate, window, and
  policy flags are set at creation, captured in the snapshot, and survive recovery unchanged —
  documented in code and `SEAM_DESIGN.md` §13.4, with a recovery test asserting every config
  field survives a snapshot + WAL-replay cycle. Any future config mutation must be a logged
  command, never snapshot-only.

## [0.1.0] - 2026-07-20

First tagged release: the obold MVP. A working, validated money-gate for Slurm —
the proven budget kernel, the daemon and management CLI over a local socket, the
GATE Lua seam, and a three-tier test story (unit → containerized Slurm → AWS
ParallelCluster). Deferred to a later release: WAL group commit (#6), the tier-2
lock-cheap read path (#7), and config durability (#8).

### Changed
- Documentation currency pass: corrected the toolchain note (CI runs Go 1.26 only, not 1.25),
  reworded the Slurm seam status from "validated on burstlab" to "validation pending" to match
  `docs/SEAM_DESIGN.md`, removed the stale "working name / one-command rename" note, aligned the
  invariant count across `CONTRIBUTING.md` and `SECURITY.md` with the canonical five in
  `CLAUDE.md`, and added CI/license badges to the README.
- Documented that `main` is not branch-protected (solo project); PRs remain the working
  convention for CI-on-change and a reviewable diff rather than an enforced gate.

### Fixed
- `obold -create` now anchors a fresh budget's window at the current clock
  (`[now, now+window)`) instead of `[0, window)`; previously every gate saw `now >= TE` and
  rejected as lapsed against the daemon's epoch clock. Regression test added.

### Added
- AWS ParallelCluster integration tier (`test/integration/`, `make integ-pcluster`): a
  build-tagged (`//go:build integration`) harness that deploys the seam to a real, already-running
  PC head node over SSH, seeds a budget, and drives the `sbatch` lifecycle on multi-node Slurm
  (funded escrow + token stamp + settle/refund + conservation; unfunded rejection). Reads cluster
  coordinates from `OBOL_INTEG_*` env and skips cleanly when unset; never provisions or destroys
  AWS resources. Partition→policy mapping modeled on the sibling `gauss/` project. See
  `docs/INTEGRATION.md`.
- Docker single-node Slurm integration tier (`test/docker/`, `make integ-docker`): builds a
  Rocky 9 image running munge + slurmctld + slurmd + slurmdbd + mariadb with the obol GATE seam
  installed, and a build-tagged Go harness (`//go:build docker_integration`) that drives real
  `sbatch` submissions. Proves the full gate → escrow → run → epilog-SETTLE → refund path against
  an actual `slurmctld` (Slurm 22.05, confirming `admin_comment` writability — SEAM_DESIGN §13
  gap #1), including a multi-user/multi-account multi-tenant test. Skips cleanly without Docker.
  See `docs/INTEGRATION.md`.
- GATE Slurm seam (`seam/`): a `JobSubmitPlugins=lua` shim (`job_submit.lua`) that gates every
  submission through obold and stamps the correlation token into `admin_comment`; a pure-Lua
  wire module (`obol_wire.lua`) mirroring `internal/wire` (IEEE crc32 verified equal to Go's,
  bidirectional frame cross-validation in `go test ./seam/lua/`); a Unix-socket transport
  (luasocket with a LuaJIT FFI fallback); and prolog/epilog scripts that BIND at job start and
  SETTLE on exit. The shim sets its own `package.path`/`cpath` (slurmctld's embedded interpreter
  does not inherit `LUA_PATH`), coerces `job_desc.time_limit` (a float) to integer seconds, and
  the transport uses a stream socket with an accumulating read — all validated against real
  Slurm 22.05. GATE-only — the burst `site_factor` plugin remains v0.3.0.
- `obol` management CLI (`cmd/obol`, `internal/cli`): talks to obold over its socket
  (decision #19 — the daemon is the single authority). Verbs: `show` (balance, burn rate,
  time-to-empty, burst, live work, conservation), `gate`, `bind`, `settle`, `ping`. The
  `--socket` flag works before or after the verb; a clean gate rejection exits 3, a transport
  error exits 1. Tests drive every verb against an in-process daemon over a real socket.
- Budget `Report(now)` inspector (`internal/budget`): a read-only, single-lock snapshot
  (balance, ledger, live counts, burst, conservation, time-to-empty) backing `obol show`.
  Additive — no transition, no money/burst-path change.
- Wire `STATUS` request/response (`internal/wire`) carrying the snapshot for `obol show`.
- Shim fail-mode model (`internal/shim`): the local open/closed gate decision the job_submit
  hook makes when obold is slow or down (`docs/SEAM_DESIGN.md` §6/§7). Hard timeout treats a
  slow daemon as down; a static per-partition class table decides fail-closed (cloud) vs
  fail-open (on-prem) with no round-trip. Tests cover both classes, the timeout boundary from
  both sides, and the unknown-partition default.
- `obold` daemon (`internal/daemon`, `cmd/obold`): a Unix-socket server wrapping the budget
  kernel. Routes `GATE`/`BIND`/`SETTLE` onto the proven kernel transitions (per
  `docs/SEAM_DESIGN.md` §12), mints unforgeable correlation tokens, holds the token↔jobid
  binding, and supplies `now` from its own clock so the kernel stays clock-free. Durable
  open/create, graceful signal shutdown. End-to-end socket tests including a concurrent gate
  storm under `-race` and a conservation-across-session assertion.
- Wire protocol (`internal/wire`): length-prefixed, crc-checked, versioned local-socket
  frames for `GATE` / `BIND` / `SETTLE` (plus `PING`), per `docs/SEAM_DESIGN.md` §8. Framing
  mirrors the kernel WAL; round-trip, multi-frame, version-mismatch, and corruption/truncation
  tests included.
- Budget kernel (`internal/budget`): atomic submit-gate with escrow/refund, per-partition
  policy flags (`bill_infra_failures`, `allow_requeue`), period lapse, job arrays with
  per-task settlement, and an explicit burst token bucket with fixed-point banking.
- Crash-safe durability: command-logged write-ahead log, snapshot recovery, and an orphan
  reconciliation janitor.
- Seam design document (`docs/SEAM_DESIGN.md`) describing the Slurm attachment.
- Project scaffold: CI (race + lint + coverage), release pipeline, governance.

[Unreleased]: https://github.com/scttfrdmn/obol/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/scttfrdmn/obol/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/scttfrdmn/obol/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/scttfrdmn/obol/releases/tag/v0.1.0
