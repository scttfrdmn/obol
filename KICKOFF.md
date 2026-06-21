# KICKOFF.md

Paste this as your first message to Claude Code, or point Claude Code at the repo and say
"follow KICKOFF.md". It is written as instructions **to** the agent.

---

You are starting work on **obol** — a hierarchical, monetary budget enforcement system for
Slurm. The management CLI is `obol`, the daemon is `obold`, the kernel is the `budget` package
at `internal/budget`.

## Read first (do not skip)

1. `CLAUDE.md` — the working agreement. The five non-negotiable invariants and the
   GitHub-is-the-plan / PRs-only workflow are there. They bind you.
2. `docs/SEAM_DESIGN.md` — the architecture and the Slurm attachment. The "why" is documented;
   honor it. Do not re-derive decisions it already settled.

## Ground truth

- **Built and proven:** `internal/budget` — atomic submit-gate, escrow/refund, per-partition
  policy flags, job arrays, an explicit burst token bucket with fixed-point banking, and
  crash-safe durability (command-logged WAL + snapshot recovery + orphan janitor). It passes
  under `-race`. **Do not rewrite it; build on it.**
- **Ahead of you:** the daemon (`cmd/obold`, currently a stub), the budget-management CLI
  (`obol`), and the Slurm seam (Lua `job_submit` shim + `site_factor` plugin), validated against
  burstlab clusters.
- **The invariants that must never regress** (full statements in `CLAUDE.md`): exact integer
  conservation; atomic gate; transitions as pure functions of `(state, command, now)`; WAL holds
  only committed transitions; burst is permission, not money.

## Session 1 — bootstrap the repo (one time)

Do these in order. Confirm each before moving on.

1. **Sanity-check what exists:** `make check` (fmt + vet + lint + race). It must be green. If it
   is not, stop and report — something drifted in transit, do not "fix" the kernel blindly.
2. **License:** replace the stub `LICENSE` with the canonical Apache-2.0 text
   (`curl -sL https://www.apache.org/licenses/LICENSE-2.0.txt -o LICENSE`) and re-add the
   copyright line at the top.
3. **Initialize and push:** `git init`, an initial commit, create the GitHub repo
   (`gh repo create scttfrdmn/obol --source=. --private`), push `main`.
4. **Seed the plan into GitHub — this is where the plan lives, not in local files.** Run
   `./scripts/bootstrap-github.sh --dry-run` first, eyeball the labels/milestones/issues, then
   run it for real. Do **not** run the issues phase twice (issues are not deduped).
5. **Protect `main`:** require CI green, require one review, disallow direct pushes, squash-merge
   only. (Use `gh api` branch-protection or the web UI.)
6. **Confirm CI is green** on the initial push before opening any PR.

After this, the GitHub project is the plan. Never create `TODO.md` / `ROADMAP.md` / `PLAN.md`.

## Then — the working loop (every change)

1. Pick or open the issue. Confirm its milestone and labels.
2. Branch: `<type>/<issue#>-<slug>` (e.g. `feat/12-gate-protocol`).
3. Implement the **smallest coherent slice**. If it touches `internal/budget`, write the test
   first.
4. **Prove it:** deterministic test + `-race` for anything concurrent + a conservation assertion.
   "Probably fine" is not proven — the race detector exists to falsify it.
5. Update `CHANGELOG.md` under `## [Unreleased]` if the change is user-facing.
6. `make check`. Then `gh pr create` — Conventional Commits title, body links the issue
   (`Closes #N`), fill the PR template's invariant checklist honestly.

## Where to start

The unblocking first issues are in milestone **`v0.1.0 — obold MVP`**:

- Start with **"Define the shim↔daemon wire protocol (GATE/BIND/SETTLE)"** (`area:protocol`,
  `p0`) — it's the contract everything else builds against, and it's pure Go, testable now.
- In parallel, the two **`status:needs-design` DECISION issues** (config durability; obol
  socket-vs-direct) want a written decision in the issue thread before code. Resolve those by
  discussion, not by inventing an answer in code.

## Hard rules (repeat of CLAUDE.md, because they matter)

- **GitHub is the plan.** No local task files. No issue → open one first.
- **PRs only.** `main` is protected. One logical change per PR.
- **Invariants never regress.** If a task is in tension with one, stop and open an issue
  describing the tension rather than relaxing it in code.
- **When a design question surfaces that `SEAM_DESIGN.md` doesn't answer, write it as an issue.**
  The kernel got correct by arguing the design before building. Keep doing that.
