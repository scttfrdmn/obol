# CLAUDE.md

Working agreement for Claude Code on this repository. Read this first, every session.

> **Naming.** Project and management CLI are both `obol` (module
> `github.com/scttfrdmn/obol`); the daemon is `obold`; the kernel package is `budget`
> (`internal/budget`). The `obol`/`obold` split is the standard command/daemon pattern
> (cf. `ssh`/`sshd`).

---

## What this is

A hierarchical, monetary budget enforcement system for Slurm. Users and groups (Slurm
accounts) map to budgets denominated in money, independent of Slurm's service units. The
gate is at job submission: a job that cannot be funded does not schedule. Budgets optionally
have a time window with banked-burst mechanics. The full model and the Slurm attachment are
in [`docs/SEAM_DESIGN.md`](docs/SEAM_DESIGN.md) — **read it before touching the daemon.**

The proven core (`internal/budget`) is already built and tested: an atomic submit-gate with
escrow/refund, per-partition policy flags, job arrays, an explicit burst token bucket, and
crash-safe durability (command-logged WAL + snapshot recovery + orphan janitor). The work
ahead is the daemon (`cmd/obold`), the Slurm seam, and the **budget-management CLI** (`obol`) — the
admin surface for creating/showing/adjusting budgets, attaching users/groups/partitions, and
`resolve`/`simulate`/audit-log verbs. The CLI is tracked under the `CLI / budget management`
milestone; single-budget verbs are buildable now, attachment/resolution verbs gate on the
hierarchy model in `v0.4.0`.

---

## The non-negotiable engineering invariants

These come from how the kernel was built and **must never regress**. A change that breaks one
is wrong even if tests are green by accident.

1. **Conservation holds, exactly.** `B0 == B + reserved + consumed + write_off` (and the
   per-array and burst-bounds variants). Money is integer units — never floats — so the check
   is `==`, not an epsilon. Any new transition asserts conservation after it.
2. **The gate is atomic.** Check-and-debit happen under one lock as one operation. Splitting
   them reintroduces the overdraft race. Every concurrent test runs under `-race`.
3. **Transitions are pure functions of `(state, command, now)`.** No transition reads the wall
   clock; `now` is always a parameter. This is what makes WAL replay deterministic — do not
   break it.
4. **The WAL only contains committed transitions.** Append after validation passes, before
   mutation. A torn tail is discarded on replay (never-committed). Recovery replays through the
   same methods used live — never a parallel apply path.
5. **Burst is permission, not money.** It is a separate bounded ledger (`0 ≤ burstPot ≤
   ceiling`); it does not participate in money conservation. Do not conflate the two.

If you find yourself wanting to relax one of these "just for now," stop and open an issue
describing the tension instead.

---

## Workflow: GitHub is the source of truth

**All planning lives in GitHub, not in local files.** Do not create `TODO.md`, `ROADMAP.md`,
`PLAN.md`, or task checklists in the repo. Work is tracked as Issues, grouped by Milestones,
labeled, and closed by PRs.

- **Every change goes through a PR.** No direct commits to `main`. `main` is protected.
- **Every PR links an issue** (`Closes #N`). No issue → open one first.
- **One logical change per PR.** Small and reviewable beats large and comprehensive.
- **Branch naming:** `<type>/<issue#>-<slug>`, e.g. `feat/12-gate-protocol`,
  `fix/34-orphan-ttl`.
- **Conventional Commits** for titles: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`,
  `perf:`, `chore:`, `ci:`. The PR title becomes the squash-merge commit.
- **Update `CHANGELOG.md`** under `## [Unreleased]` in the same PR (Keep a Changelog format).
  A user-facing change with no changelog entry is incomplete.

Use the `gh` CLI for everything: `gh issue create`, `gh pr create`, `gh pr view`. Never
hand-edit issue state in a file.

## SemVer 2.0.0

Pre-1.0 (`0.y.z`): minor bumps may break; patch is fixes. The public surface that SemVer
governs is the daemon's wire protocol and the `internal/budget` exported API as consumed by
`cmd/`. Releases are cut from `main` by tagging `vX.Y.Z`; the release workflow builds artifacts.
The changelog's `Unreleased` section becomes the release notes.

---

## Go practices (2026)

- **Go 1.25 floor** (`go` directive `1.25`); developed on 1.26, and CI runs both 1.26 and the
  1.25 floor. The directive is a *minimum*, so a `go 1.26` directive would make the 1.25 job
  unbuildable — the floor is what keeps "don't use features newer than the floor" testable. Do
  not use language features newer than the floor without bumping the directive deliberately (and
  the CI matrix with it).
- **Layout:** `cmd/<binary>` for mains, `internal/` for everything not meant for external
  import. The kernel stays in `internal/budget`. No premature `pkg/`.
- **Errors:** wrap with `%w`, sentinel errors as package vars, no `panic` in library code except
  as a tripwire for a broken invariant (see `settleTask`'s per-escrow panic — that pattern is
  intentional and allowed).
- **Concurrency:** the kernel's mutex discipline is load-bearing. New shared state declares how
  it's synchronized. Anything concurrent gets a `-race` test.
- **Tests:** table-driven where it helps; deterministic transition tests *and* a concurrent
  fuzz/storm for anything touching shared state. Assert the invariants, not just outputs. Target
  meaningful coverage on `internal/budget` (the money path), not a vanity percentage.
- **Formatting/lint:** `gofmt` + `golangci-lint` (v2 config in `.golangci.yml`) must pass. Run
  `make check` before pushing.
- **Dependencies:** standard library first. Every third-party dep needs a one-line justification
  in the PR. Apache-2.0-compatible licenses only.

## Common commands

```
make build        # build obold
make test         # go test ./...
make race         # go test -race ./...  (REQUIRED before any concurrency PR)
make lint         # golangci-lint run
make cover        # coverage report
make check        # fmt-check + vet + lint + race  (run before pushing)
make tidy         # go mod tidy + verify
```

---

## How to approach a task here

1. Read the relevant part of `docs/SEAM_DESIGN.md`. The "why" is documented; honor it.
2. Find or open the issue. Confirm the milestone and labels.
3. Branch, implement the smallest coherent slice, write the tests first if it touches the
   kernel.
4. Prove it: deterministic test + `-race` for concurrency + conservation assertion.
5. Update `CHANGELOG.md` Unreleased.
6. `make check`, then `gh pr create` linking the issue.

When a design question surfaces mid-task that the seam doc doesn't answer, **stop and write it
as an issue** rather than inventing an answer in code. The kernel got correct by arguing the
design before building; keep doing that.
