# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/scttfrdmn/obol/commits/main
