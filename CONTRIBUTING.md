# Contributing

Read `CLAUDE.md` first — it is the working agreement and it applies to humans and agents alike.

## Planning lives in GitHub

No local task files. Work is **Issues** grouped by **Milestones**, labeled, closed by **PRs**.
If you're about to do work that isn't an issue, open the issue first (`gh issue create`).

## The PR flow

1. Branch from `main`: `<type>/<issue#>-<slug>` (e.g. `feat/12-gate-protocol`).
2. Make one logical change. Tests first if it touches `internal/budget`.
3. Prove it: deterministic test + `-race` for anything concurrent + conservation assertion.
4. Update `CHANGELOG.md` under `## [Unreleased]` for user-facing changes.
5. `make check`.
6. `gh pr create`, title in Conventional Commits form, body links the issue (`Closes #N`).

This is a solo project, so `main` is not branch-protected — but the PR flow is still the
convention: it runs CI on the change and gives a reviewable diff before self-merge. Keep CI
green. Merges are squash; the PR title becomes the commit.

## Conventional Commits

`feat:` `fix:` `docs:` `test:` `refactor:` `perf:` `chore:` `ci:`. A `feat:` or `fix:` that is
user-facing must carry a changelog entry in the same PR.

## Versioning

Semantic Versioning 2.0.0. Pre-1.0, minor may break. Releases are tagged `vX.Y.Z` from `main`;
the `Unreleased` changelog section becomes the release notes.

## The invariants

Do not regress any of the five invariants — exact integer conservation, gate atomicity,
transitions as pure functions of `(state, command, now)`, a WAL that holds only committed
transitions, and burst-as-permission (bounded, separate from money). They are spelled out in
full in `CLAUDE.md`, which is canonical. If a change is in tension with one, open an issue
describing the tension rather than relaxing the invariant in code.

## License of contributions

By contributing you agree your contributions are licensed under Apache-2.0.
