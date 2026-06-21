## What

<!-- One sentence: what this PR changes. -->

Closes #

## Why

<!-- Link to the design rationale in docs/SEAM_DESIGN.md if relevant. -->

## Invariant check

- [ ] Conservation still holds (money is integer, asserted in tests)
- [ ] Concurrency changes covered by a `-race` test
- [ ] Transitions remain pure functions of `(state, command, now)`
- [ ] `CHANGELOG.md` Unreleased updated (if user-facing)
- [ ] `make check` passes locally
