# obol

> **Working name.** `obol` (an ancient small-denomination coin) is a placeholder.
> See `CLAUDE.md` for the one-command rename.

Hierarchical, monetary budget enforcement for [Slurm](https://slurm.schedmd.com/).

Users and groups (Slurm accounts) map to budgets denominated in **money**, independent of
Slurm's service units. The enforcement point is job submission: a job that cannot be funded
does not schedule. Budgets optionally carry a time window with banked-burst mechanics — idle
time banks burst permission; concurrency spends it.

## Status

| Component | State |
|-----------|-------|
| `internal/budget` — the kernel | **built & tested** (conservation + concurrency proven under `-race`, crash-safe WAL durability) |
| `cmd/obold` — the sidecar daemon | stub; tracked in milestone `v0.1.0` |
| Lua `job_submit` shim + `site_factor` plugin | designed (`docs/SEAM_DESIGN.md`); validated on burstlab clusters |

The architecture — why a sidecar daemon, the three-tier latency model, the `admin_comment`
correlation token, the owned-vs-rented partition policy axis — is documented in
[`docs/SEAM_DESIGN.md`](docs/SEAM_DESIGN.md).

## Build

```
make build     # -> bin/obold
make check     # fmt + vet + lint + race  (what CI enforces)
```

Requires Go 1.26 (CI also runs 1.25, the previous supported major).

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
