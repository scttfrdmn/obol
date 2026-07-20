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

### Added
- Budget kernel (`internal/budget`): atomic submit-gate with escrow/refund, per-partition
  policy flags (`bill_infra_failures`, `allow_requeue`), period lapse, job arrays with
  per-task settlement, and an explicit burst token bucket with fixed-point banking.
- Crash-safe durability: command-logged write-ahead log, snapshot recovery, and an orphan
  reconciliation janitor.
- Seam design document (`docs/SEAM_DESIGN.md`) describing the Slurm attachment.
- Project scaffold: CI (race + lint + coverage), release pipeline, governance.

[Unreleased]: https://github.com/scttfrdmn/obol/commits/main
