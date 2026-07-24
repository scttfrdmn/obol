# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Slurm 25.11 CI coverage** — a `managed` generation (Rocky 9, Slurm 25.11.1)
  added to the multi-gen tier (`test/docker/multigen_test.go`), covering the version
  AWS PCS and ParallelCluster ship (a live PC ran the seam on 25.11.4, #131) but that
  was outside obol's tested 22.05/23.11/24.05 set. A new `integ-multigen` GitHub
  Actions workflow runs the multi-gen seam tests **weekly** and on **manual dispatch**
  (defaulting to the `managed` gen) — the build compiles Slurm from source (~10-20
  min/image), so it deliberately does not run on every PR.

### Changed
- The GATE shim (`seam/lua/job_submit.lua`) now searches `/usr/local/lib/lua/<ver>`
  for luasocket's C modules and honors `OBOL_LUA_CPATH` for extra patterns (#137).
  A luasocket built from source (the fallback on managed hosts without a
  `lua-socket` package — e.g. AWS ParallelCluster) installs to `/usr/local/lib`,
  which the shim's cpath previously omitted, so `socket.unix` wouldn't load and
  every gate failed "no usable socket backend". Also adds the 5.3 `/usr/lib` and
  `/usr/local/lib` dirs. (The deeper "don't assume a Lua socket backend at all"
  work remains open under #137.)

### Added
- `obold -socket-group` / `-socket-mode` (#136): set the listen socket's group
  (name or gid) and octal mode at listen time, so a **non-root slurmctld** (e.g.
  ParallelCluster runs it as `slurm`) can connect — `connect(2)` on a Unix socket
  needs write permission, which the default root-owned socket denies. Recommended
  pairing is `-socket-group slurm -socket-mode 0660`. This only widens who can drive
  the gate/bind/settle lifecycle and read verbs; **mutating verbs stay gated on
  `SO_PEERCRED`**, so grouping the socket does not confer admin. The ParallelCluster
  bundle now passes these flags (with an ExecStartPost fallback for older obold).
- `docs/feasibility-parallelcluster.md`: a feasibility write-up for running obol on
  AWS ParallelCluster. Conclusion: feasible today with **one small seam divergence
  and no kernel changes** — the head node is customer-owned with root, so the seam
  attaches via ParallelCluster's `CustomSlurmSettings`; the PC-specific work is
  packaging (a bootstrap custom action + `-state-dir` on managed shared storage), the
  BIND re-home below, and a head-node-replacement drill. Documents the
  confirmed-vs-unknown split and the recommended attachment model.
- `deploy/parallelcluster/` — a reproducible bootstrap bundle for running obol on
  AWS ParallelCluster (#131): a **phase-aware** `install-obol.sh` (`--phase files` at
  `OnNodeStart` lays the seam down before slurmctld; `--phase daemon` at
  `OnNodeConfigured` starts obold), a sample `cluster.yaml` carrying the seam via
  `CustomSlurmSettings`, and a README. The release archive now also bundles the seam +
  this deploy dir so a release is self-contained (goreleaser `archives.files`).
  **Validated end to end on a live cluster** (PC 3.15.1, alinux2023, Slurm 25.11):
  GATE→BIND→SETTLE with conservation intact, and unfunded-job rejection. The live run
  surfaced four stock-AMI gaps the installer now handles — install order (files
  before slurmctld), building a Lua socket backend from source (no luasocket/FFI on
  the AMI), `scontrol` on the prolog PATH, and group-writable socket for the `slurm`
  user — plus two follow-up daemon/seam issues (#136 `obold -socket-group`, #137 a
  robust Lua transport backend).

### Changed
- ParallelCluster attachment model reflects how PC actually manages Slurm config:
  directives attach via **`CustomSlurmSettings`** (not a hand-edited `slurm.conf`,
  which PC overwrites on update), and PC **deny-lists `Prolog`/`Epilog`** — so obol's
  BIND step re-homes from `Prolog` to **`PrologSlurmctld`** (head-node, not
  deny-listed). GATE (`JobSubmitPlugins`) and SETTLE (`JobCompType`/`JobCompLoc`)
  attach unchanged. This is the one seam divergence on PC; the money kernel and wire
  protocol are untouched.
- README/docs cleanup: removed the README Branding section; the quickstart and
  verify steps now install the binaries to `PATH` and invoke bare `obold`/`obol`
  (was `bin/obold`), and dropped the redundant `--socket /run/obol/obold.sock`
  everywhere — `obol` uses that default automatically, so `--socket` is now shown
  only where a non-default socket is actually used (the developer demo). Docs note
  it's optional.
- Docs review follow-ups: fixed the contradictory node-type pricing example in
  `configuration.md` (valid divisible example + an explicitly-labeled invalid one
  with the exact rejection message, plus a note on choosing money-unit precision);
  clarified the **acknowledged-vs-physically-synced** boundary in `operations.md`
  (the gate acks on the in-memory change, before its group-commit `fdatasync`; a
  power-loss window self-heals into a consistent state); refreshed two stale
  references (README "docs backlog" → the production-readiness guide;
  `installation.md` "once written" parenthetical removed); trimmed the README
  maturity table's CLI row to a scannable summary linking the CLI reference; and
  removed the terminal-recordings item (declined as unnecessary for this project).

### Changed
- README overhaul: the opening now states the audience, the operator problem, the
  reservation mechanism, and how Obol differs from native Slurm accounting/QOS
  (with a worked $10k-lab example); a **Maturity & compatibility** section
  replaces the stale status table and reflects v0.14.0, the full 19-verb CLI, and
  the Docker-validated seam (22.05/23.11/24.05); the "Try it" protocol demo is
  replaced by a real `sbatch`-based quickstart (the wire-primitive demo is kept as
  a developer aside). Removed the obsolete top-level `KICKOFF.md`.

### Added
- `docs/production-readiness.md` — the compatibility matrix (Slurm 22.05/23.11/24.05,
  Rocky 8/9/10, Go 1.26, arch, wire/state policy), an honest **validation-status**
  table (CI runs the unit tier only; the Docker/multi-gen/ParallelCluster tiers are
  build-tag-gated and validated locally), Mermaid diagrams of the gate→bind→settle
  lifecycle and the two orphan janitors, a go-live checklist, and known limitations
  (#123, codeable parts; terminal recordings still pending).
- `docs/README.md` — a documentation map routing by intent (evaluating / trying /
  deploying / operating / contributing), and `docs/cli-reference.md` — every
  `obol` verb with flags, exit-code convention, and which verbs require admin.
  The top-level README now points at the map (#122).
- `docs/operations.md` — the run-it-safely guide: the durability model (WAL group
  commit + snapshot, why a crash is safe), backup/restore, recovery after
  daemon/controller/network/state-dir failure, the two orphan janitors
  (automatic unbound-token TTL sweep + `squeue | obol reconcile`), per-partition
  fail-open/closed, monitoring/health (`ping`, conservation, `list`), upgrade &
  rollback under pre-1.0 wire/state compatibility, and the `SO_PEERCRED` admin
  model — plus an operational checklist. Linked from the README (#121).
- `docs/installation.md` + `docs/configuration.md` — the deploy path: getting the
  binaries (release/source), the full `obold` flag reference, state-dir layout,
  a systemd unit, socket permissions + the admin model, Slurm wiring, and verify
  steps; plus the complete `obold.json` schema reference (accounts, rate, window,
  access lists, node-type pricing, burst, admins), validation rules, and the
  runtime-change verbs. Linked from the README (#120).
- `docs/concepts.md` — a pedagogy guide that builds up Obol from one worked
  example (the $10k lab: reserve → admit → settle → refund) through balances vs.
  reservations, costing, hierarchy, windows, banked burst, partition policy,
  conservation, **why money is stored as integer units**, and a section on **how
  Obol relates to Slurm accounting/QOS/fair-share** (gate-before vs. measure-after;
  reuses the account tree). Linked from the README's new Documentation section (#119).
- Project branding in `docs/assets/` (hero banner, logo, social-preview card,
  sticker); README now leads with the hero banner and documents the assets.

## [0.14.0] - 2026-07-22

Runtime burst administration (#99): burst is no longer set-once in the config
file. Minor bump — additive `SET_BURST` wire message + burst fields on `create`;
a new **logged** kernel transition (`SetBurst`) so live burst changes replay in
order and survive recovery. Burst is permission, not money, so none of this
touches conservation.

### Added
- `obol set-burst` changes burst on an existing account at runtime (#99):
  `--ceiling-pct P [--draw-cap N]` enables/re-ceilings, `--disable` turns it off.
  Backed by a new **logged** kernel transition `SetBurst` (like `SetRate`/
  `SetWindow`), so the change is replayed in order and survives recovery — burst
  config is no longer set-once-at-creation. Lowering the ceiling below the banked
  pot clamps it; disabling zeroes the bucket; both forfeit only permission tokens
  (burst is permission, not money — conservation untouched). Admin-gated. New
  `SET_BURST` wire message.
- `obol create` can enable burst on a runtime-created account (#99):
  `--burst-ceiling-pct` (turns burst on) and `--burst-draw-cap`, mirroring the
  `obold-config.json` fields. Set before the account's initial snapshot, so it
  survives a restart exactly like a config-created burst account.

## [0.13.0] - 2026-07-22

Completes the orphan-janitor story: a `reconcile` admin verb drives the kernel's
`SweepOrphans` from a live-job feed, reclaiming started escrows whose job vanished
(lost completion, or a crash that stranded the routing) — the complement to the
unbound-token TTL sweep. Minor bump: additive `RECONCILE` wire message; kernel
change scopes `SweepOrphans` to started escrows so the two janitors don't race.

### Added
- `obol reconcile` + a `RECONCILE` wire verb wire the kernel's `SweepOrphans` to a
  live-job feed (#97): `squeue -h -o %A | obol reconcile` hands the daemon the set
  of currently-live Slurm job ids, and it full-refunds any **started** escrow whose
  job is no longer live — the lost-completion class, and the started-orphan a crash
  can strand once its routing is gone. Admin-gated. `SweepOrphans` now sweeps only
  started escrows/tasks, so it and the unbound-token janitor (#15) partition the
  orphan space cleanly and never race. Completes the janitor story: both orphan
  classes are now reclaimed.

## [0.12.1] - 2026-07-21

Multi-source funding is now reachable from a real `sbatch` (via
`--comment obol-sources=...`), not just the diagnostic CLI. Patch: seam/shim
plumbing only — no wire, kernel, or API change; a job without the token behaves
exactly as before.

### Added
- Multi-source funding is reachable from a real `sbatch` (#98): a job names its
  ordered funding accounts in `--comment` as `obol-sources=grant,startup`, and the
  GATE shim parses that into the gate's `sources` list (absent → single
  `--account`, unchanged). `--comment` being user-editable grants nothing — the
  daemon authorizes every source, so a submitter can only draw from accounts they
  already have access to. Validated end to end in the Docker tier (a job draining
  one budget and spilling to the next, then recovering on cancel).

## [0.12.0] - 2026-07-21

Multi-source funding is complete: job arrays can now draw from multiple account
budgets (whole-tasks split), alongside the 1:1 support shipped in 0.10.0. Minor
bump; no wire change (reuses the `sources` field), kernel untouched — each leg is
an ordinary array escrow and each budget conserves on its own.

### Added
- Multi-source funding now supports job arrays (#96): an `sbatch --array` funded
  by multiple sources is split by **whole tasks** in ordered fallback — source 1
  funds the first `floor(B/(rate·walltime))` task indices, the next source the
  overflow, each leg an ordinary array escrow over a contiguous index range.
  All-or-nothing: rejected unless the sources jointly fund all N tasks. Per-task
  bind/settle routes to the one leg owning that index; each budget conserves on
  its own. Completes the multi-source feature (1:1 jobs shipped in 0.10.0).

## [0.11.0] - 2026-07-21

Job arrays end to end. An `sbatch --array` submission is now budget-enforced
through the whole seam: gated as one escrow, then bound and settled per task, with
each task drawing and refunding its own slice. Minor bump: additive
`array_task`+`idx` wire fields and `obol bind/settle --idx`; the array kernel was
already built, so this is seam/daemon plumbing — 1:1 jobs are unchanged.

### Added
- Single-source job arrays now flow through the seam end to end (#103, toward
  #96). The GATE shim (`job_submit.lua`) reads `--array` and gates all N tasks as
  one escrow; the wire `BindRequest`/`SettleRequest` carry `array_task`+`idx`; the
  daemon drives the array kernel per task (`handleBind`→`StartTask`,
  `handleSettle`→the matching `*Task`), dropping token routing only when the last
  task settles; and the prolog/jobcomp scripts bind and settle each task by its
  `ArrayTaskId` (resolved via `scontrol` in the prolog, from `ARRAYTASKID` in
  jobcomp). `obol bind`/`obol settle` gained `--idx`. So `sbatch --array=0-3` now
  escrows the whole array, runs, and settles each task's slice with the budget
  conserving throughout — validated in the Docker tier. 1:1 jobs are unchanged.

## [0.10.1] - 2026-07-21

Validation-only patch: the obol seam is now proven against all three burstlab
Slurm generations built from source. No product code changed — the daemon,
kernel, wire protocol, and CLI are identical to 0.10.0; this adds a test tier.

### Added
- Multi-generation Docker Slurm integration tier (#16): `make integ-docker-multigen`
  builds Slurm **from source** at each burstlab generation's exact version
  (22.05.11 / 23.11.10 / 24.05.5) on its matching Rocky base, and runs the GATE→
  SETTLE money path + the `admin_comment` token round-trip against each — resolving
  the per-generation §10 ABI question. Recipe matched to burstlab's packer AMIs
  (`Dockerfile.slurm-src`, parameterized by base/version). Opt-in, local, behind
  the `docker_multigen` build tag (not in CI; ~10–20 min/image). All three
  generations — Gen 1 (22.05.11/Rocky 8), Gen 2 (23.11.10/Rocky 9), Gen 3
  (24.05.5/Rocky 10) — build, boot, and pass the money path + admin_comment probe;
  the shared entrypoint was made base-agnostic (munge key tool and mariadb daemon
  are named differently across Rocky 8/9/10).

## [0.10.0] - 2026-07-21

Multi-source funding. A single Slurm job can now draw from **multiple account
budgets** in ordered fallback — the last piece of budget-membership scope the
flat per-account model deferred (#54). Minor bump: additive `sources` wire field
and gate orchestration; the kernel is unchanged (each funding leg is an ordinary
single-budget escrow, and each budget conserves on its own), and single-source
submissions behave exactly as before.

### Added
- Multi-source funding (#54): one Slurm job may now draw from **multiple account
  budgets** via an ordered `sources` list (ordered fallback — drain the first,
  spill to the next). The gate places one escrow per source (each funding a
  contiguous whole-second slice of the job), **all-or-nothing**: if any leg can't
  be funded, the already-placed legs are rolled back and the job is rejected.
  Settle fans out across the legs so total billed equals the job's true cost and
  an early exit refunds the later sources; node failure applies each budget's own
  bill/write-off policy to its slice. Each budget still conserves on its own (no
  cross-budget invariant); a partial gate self-heals via the unbound-token janitor
  (no new journal). Single-source submissions are unchanged. 1:1 jobs only this
  round (arrays stay single-source). `obol gate` gains a repeatable `--source`
  flag to exercise it (the Slurm shim convention is a follow-up).
- Multi-source funding groundwork (#54): a `sources` field on the gate wire
  request (ordered list of account budgets to fund one job) and the
  ordered-fallback funding-plan computation. Each source funds a contiguous
  whole-second time slice of the job at the full rate — quantized to `floor(B/c)`
  seconds so every leg is an ordinary single-budget escrow (no kernel change) and
  each budget keeps its own conservation. Not yet wired into the gate (next
  change); single-source submissions are unaffected.

## [0.9.0] - 2026-07-21

Burst dispatch gate + janitor hardening. This delivers the burst dispatch path
end to end (#14): a lock-free per-job "may this start now, or hold at priority
0?" decision on the tier-2 read view, per-account burst configuration in
`obold-config.json`, the `DISPATCH` wire verb, the `obol dispatch` CLI, and a
reference `site_factor` C plugin. It also closes the submit→start orphan window
with the unbound-token TTL janitor (#15). Minor bump: additive `DISPATCH` wire
message and new kernel methods (`MayDispatch`, `SweepUnbound`, `NewDurableBurst`);
the money ledger and prior behavior are unchanged, and burst stays off unless
configured.

### Added
- Unbound-token TTL janitor (#15): `obold` periodically reclaims escrows minted at
  the gate but never bound to a job id — the submit→start orphan window, where a
  daemon crash leaves reserved money the jobid-based sweep can't match. The kernel
  `SweepUnbound(ttl, now)` full-refunds any never-started escrow older than the
  TTL (it provably never ran); the escrow's submit time is persisted so the age
  survives recovery. New `obold` flags `-unbound-ttl` (default 15m; 0 disables)
  and `-sweep-interval` (default 1m).
- Reference `site_factor` burst dispatch plugin (`seam/plugin/obol_site_factor.c`
  + README): the C plugin the `priority/multifactor` scheduler calls to hold a
  pending job at priority 0 when it has no burst headroom. It reads the job's
  token, asks obold `DISPATCH`, and fails open on any error. Shipped as documented
  compilable reference (not built/tested in CI; the Slurm hook has no Lua binding
  and can't run in the RPM-based Docker tier) — the tested equivalent is
  `obol dispatch` / `handleDispatch`. Completes #14.
- `obol dispatch` (alias `obol may-dispatch`) — the CLI face of the burst dispatch
  query: reports whether a pending job would dispatch now or hold at priority 0,
  with the rate, reservation, and projected pot. Exit 0 = would dispatch, 3 =
  would hold. This is the CI-tested equivalent of what the `site_factor` plugin
  asks the daemon (#14).
- `DISPATCH` wire message + daemon `handleDispatch`: the burst dispatch query the
  `site_factor` plugin needs ("may this pending job start now, or hold at priority
  0?"). Read-only, visibility-scoped, resolves the job's rate like the gate, and
  answers lock-free via `MayDispatch` — the tier-2 hot path (#14).
- Burst is now configurable per account in `obold-config.json`: `burst_enabled`,
  `burst_ceiling_pct` (0–1, pot ceiling as a fraction of the allocation), and
  `burst_draw_cap` (max tokens one job may reserve). Enabling burst lets jobs bank
  idle capacity and later dispatch above the sustainable rate (concurrency
  shaping; burst is permission, not money). Config is applied before the initial
  snapshot so it survives a restart. New kernel constructor `NewDurableBurst`
  (#14). Runtime-created accounts (`obol create`) remain non-burst for now.
- Kernel `Budget.MayDispatch` — a lock-free burst dispatch query answering "would
  this job get the burst headroom to start now, or must it hold?" for the
  `site_factor` priority path (#14). It reads the tier-2 `ReadView` (extended to
  carry the burst-projection inputs) and routes through the same pure verdict
  helper as the locked gate, so the lock-free answer can never drift from what
  `Start` actually does (asserted by a `MayDispatch`-vs-`Start` agreement matrix).

## [0.8.0] - 2026-07-21

Money movement between budgets, and the kernel primitive under it. This closes
the CLI / budget-management milestone: admins can now reallocate funds across
accounts with `obol transfer`, atomically and crash-safely. Minor bump: additive
`TRANSFER` wire message and new kernel transitions (`Withdraw`, transfer-tagged
top-up/withdraw). Conservation (invariant #1) now holds *across two budgets* for a
completed or recovered transfer; the money ledger's single-budget behavior is
unchanged. Transfers require `obold -config`.

### Added
- `obol transfer --from A --to B (--amount N | --all)` — move money between two
  account budgets, admin-gated. The move is **atomic across a crash**: a daemon
  transfer journal plus per-leg WAL tagging (`Xfer`) let restart recovery complete
  or abort an interrupted transfer, so money is never created or destroyed
  (conservation holds across both budgets). New `TRANSFER` wire message. Closes
  the CLI / budget-management milestone (#25).
- Kernel `Budget.Withdraw` — the money-symmetric inverse of `TopUp`: lowers both
  `B` and `B0`, capped at available balance (never reserved/consumed), logged and
  replayed on recovery, allowed regardless of lifecycle status. Building block for
  `obol transfer` (#77, toward #25).
- Transfer correlation on the WAL: an optional `Xfer` tag on topup/withdraw
  commands (`TopUpXfer`/`WithdrawXfer`) plus a read-only `budget.HasXfer` lookup,
  so a transfer's two legs are exactly-once identifiable across two budget WALs.
  Audit log renders the `withdraw` kind and its `xfer` tag.

## [0.7.0] - 2026-07-21

Runtime budget administration + pre-submit estimation. Admins can now create
accounts and grant/revoke access live (no restart), and users can dry-run a
job's funding and runway before submitting. Minor bump: additive
`SIMULATE`/`CREATE`/`ATTACH` wire messages, a read-only `budget.Simulate` kernel
method, and a runtime-mutable registry (RWMutex + on-disk discovery). The money
ledger and prior behavior are unchanged; runtime create/attach require
`obold -config`.

### Added
- `obol create` (#70) and `obol attach`/`obol detach` (#23) — live registry mutation. Admins can
  create a new account's budget and grant/revoke user/group access **at runtime, without a daemon
  restart**. The registry gained a `sync.RWMutex` (reads take RLock, mutations Lock; per-budget
  kernel locks unchanged). Runtime changes are **durable and restart-safe**: a created account
  persists via its own WAL/snapshot (as budgets already do) plus a daemon-owned `account.json`
  (name + access lists — what the kernel snapshot deliberately doesn't hold); on startup the daemon
  **discovers** existing account dirs under `-state-dir` and loads them, with `-config` only
  bootstrapping accounts not already on disk (no config-file rewriting). Both verbs are admin-gated
  by peer credentials. The kernel is untouched (`create` reuses `NewDurable`; access is a daemon
  concern). Requires `obold -config` (the single-budget path has no state dir for live mutation).
  Validated end-to-end incl. restart survival.

### Added
- `obol simulate` / `obol estimate` — will-it-fund + runway (issue #21): given an account and a
  hypothetical job (`--time-limit`, optional `--partition`/`--cpus`/`--gpus`/`--mem`), reports the
  cost, whether the gate would admit it now, the deny reason if not, and the budget's projected
  **runway** (time-to-empty at the current balance and rate) — **committing nothing**. Backed by a
  new read-only `budget.Simulate` kernel method that mirrors the gate's solvency/window/rate-ceiling
  checks *and* the dispatch-time burst-headroom check against a projected accrual, without debiting
  or banking. Read-only, visibility-scoped; exit 3 when it would not fund.

## [0.6.0] - 2026-07-21

Live budget administration + diagnostics. Admins can adjust an account's rate and
window at runtime (`set-rate`/`set-window`, the first logged config mutations —
satisfying the issue #8 ordering guarantee), and anyone with read access can
dry-run the gate's decision for a submission (`resolve`). Minor bump: additive
`SET_RATE`/`SET_WINDOW`/`RESOLVE` wire messages and the `SetRate`/`SetWindow`
kernel transitions; the money ledger and prior behavior are unchanged.

### Added
- `obol resolve` — dry-run the gate decision (issue #24): given `--account [--partition]
  [--time-limit] [--uid]`, reports which budget resolves, the effective rate and its source
  (node-type worst-case / TRES / flat), current balance, the cost the job would escrow, the access
  verdict, and whether the gate would admit — **escrowing nothing**. A diagnostic for "why did/
  didn't this job match a budget?" Read-only, visibility-scoped by peer credentials; exit 3 when
  it would be rejected (mirrors `gate`).
- `obol set-rate` / `obol set-window` — live budget config mutation (issue #20): admins can change
  an account's flat cost rate and time window without a restart. These are the first **logged
  config transitions**, satisfying the issue #8 design: `SetRate`/`SetWindow` are WAL commands
  replayed in order, so a submit before a change replays at the old value and one after at the new
  — never a snapshot-only change. `set-rate` affects future flat-rate submits only (live escrows
  keep the rate they froze at submit); `set-window` gates future submits while live escrows settle
  normally across the change. Both leave the money ledger untouched (conservation preserved),
  are admin-gated by peer credentials, and render in `obol log`. `set-window` accepts
  `--window <dur>` or explicit `--start/--end` (RFC3339 or epoch). Runtime account *creation* is
  deferred to #70. Validated end-to-end.

## [0.5.0] - 2026-07-21

Node-type cost model + audit log. Cost now attaches to the node Slurm binds: a
partition's job escrows the worst-case node rate at submit (placement unknown)
and is repriced down to the actual node's rate at dispatch (via the prolog/BIND),
so heterogeneous partitions bill correctly. `obol log` renders the WAL as a
human-readable audit trail. Minor bump: new `Reprice` transition, `LOG` wire
message, `BIND` `NodeType` field, and node-type config — all additive;
partitions without node-type config keep prior pricing.

### Added
- Node-type cost true-up at dispatch (issue #65): `BIND` now carries the actual node type
  (`BindRequest.NodeType`); when node-type pricing is configured, the daemon reprices the escrow
  from the worst-case estimate down to the bound node's rate — via the kernel `Reprice` transition,
  before `Start`. The prolog resolves the node type from `SLURM_JOB_NODELIST → ActiveFeatures` (or
  `OBOL_NODETYPE`) and passes `obol bind --node-type`. Validated end-to-end on real Slurm 22.05: a
  job escrows the worst-case rate at submit, reprices down when placed, and settles at the trued
  rate (`obol log` shows submit → reprice → start → settle). `docs/SEAM_DESIGN.md` §5 updated.
- Node-type cost config + worst-case escrow (issue #65): `obold -config` gains `node_types`
  (name → rate, expressible per `s`/`m`/`h` — normalized to integer units/second at load, with a
  clear error if a rate doesn't divide cleanly) and `partitions` (name → the node types it can
  place on). When a submission's partition has node types configured, the gate escrows the
  **worst-case** (max) rate over that set — since the node isn't chosen yet — instead of the
  TRES/flat rate. Partitions without node-type config keep the existing pricing. (The BIND-time
  true-up to the actual node's rate follows in the next PR, using the `Reprice` transition.)
- Budget `Reprice(jobID, newRate, now)` kernel transition (issue #65): lowers a live escrow's cost
  rate before it starts, refunding the over-reservation (`Reserved → B`, `B0` unchanged). This is
  the node-type cost true-up — the gate escrows the worst-case node rate at submit (placement
  unknown), and once Slurm binds the real node the daemon reprices to its cheaper rate. May only
  *lower* the rate (worst-case escrow guarantees `newRate ≤ current`; a raise is rejected to keep
  the no-overdraft invariant), and only before `Start`. Logged + replays on recovery; conservation
  asserted before/after and across a crash. Tests under `-race`.
- `obol log` — transaction/audit view (issue #22): renders an account's WAL as a time-ordered
  list of transitions (submit/start/settle/lapse/topup) with amounts, rates, and runtimes. The WAL
  is already an append-only audit trail, so this is a read-only render — `budget.Budget.Log()` /
  `ReadLog` read the WAL file directly (no lock, no live-state replay), the daemon exposes it via a
  `LOG` wire message, and it is visibility-scoped by peer credentials exactly like `show`/`list`.

## [0.4.0] - 2026-07-21

Live budget administration with real authorization. `obol topup` adds money to a
running account (a logged, conservation-preserving kernel transition), `obol list`
enumerates accounts, and management commands are now authorized by the
connection's kernel-verified peer identity (`SO_PEERCRED`) — mutating verbs
require an admin, reads are visibility-scoped. Minor bump: additive `TOPUP`/`LIST`
wire messages and the `TopUp` transition; a new `golang.org/x/sys` dependency;
authorization is off by default (opt-in via `admin_users`/`admin_groups`), so
existing deployments are unaffected.

### Added
- Docker tier validates topup + peer-cred authz on real Slurm (issue #59): with `admin_users:
  ["root"]` configured, root tops up an account (balance/allocation grow, conservation holds), a
  non-admin user who *can reach the socket* is rejected by `SO_PEERCRED`, and `obol list` shows
  the accounts — proving authorization keys on the kernel-verified uid, not socket permissions.
- `obol topup` / `obol list` + peer-credential authorization (issue #59): the daemon gains
  `TOPUP` (admin-only, adds money to a live account) and `LIST` (enumerate visible accounts) wire
  messages. **Management commands are now authorized by the connection's kernel-verified peer
  identity** (`SO_PEERCRED` on Linux; the spoofable wire `uid` is never used for authz). Mutating
  verbs (topup) require an admin — configured `admin_users`/`admin_groups`, or root — and read
  verbs (`show`/`list`) are visibility-scoped: admins see all, others see accounts they belong to
  plus open budgets. When no admin list is configured, enforcement is off (socket permissions are
  the boundary), preserving prior behavior. New dependency: `golang.org/x/sys` (peer creds; no
  portable stdlib equivalent).
- Budget `TopUp(amount, now)` kernel transition (issue #59): adds money to a live budget by
  raising **both** the balance `B` and the allocation anchor `B0`, so conservation holds exactly.
  Add-only (positive amount), works regardless of lifecycle status (an admin action, not a
  submit), and is a **logged** transition — the amount rides the WAL and replays on recovery (the
  first money-*increasing* command, per the #8 immutable-config decision). Covered by
  conservation, fund-new-work, lapsed, reject-non-positive, and recovery tests under `-race`.

## [0.3.0] - 2026-07-21

Per-account budgets. obold moves from a single pot to a registry of independent
budgets, one per Slurm account (`obold -config`), with resolution and optional
access enforcement — removing the single-budget limitation. The kernel is
untouched (each account conserves independently); the flat per-account model
superseded the originally-planned account-tree rollup. Minor bump: additive
`-config` and an additive `Account` field on the STATUS wire message; the
single-budget path is unchanged when `-config` is absent.

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

[Unreleased]: https://github.com/scttfrdmn/obol/compare/v0.14.0...HEAD
[0.14.0]: https://github.com/scttfrdmn/obol/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/scttfrdmn/obol/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/scttfrdmn/obol/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/scttfrdmn/obol/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/scttfrdmn/obol/compare/v0.10.1...v0.11.0
[0.10.1]: https://github.com/scttfrdmn/obol/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/scttfrdmn/obol/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/scttfrdmn/obol/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/scttfrdmn/obol/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/scttfrdmn/obol/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/scttfrdmn/obol/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/scttfrdmn/obol/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/scttfrdmn/obol/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/scttfrdmn/obol/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/scttfrdmn/obol/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/scttfrdmn/obol/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/scttfrdmn/obol/releases/tag/v0.1.0
