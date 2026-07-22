# Operating Obol

Running Obol safely once it's deployed: durability and backup, recovery after a
failure, the orphan janitors, monitoring, upgrades, and the admin model. This
assumes you've installed and configured it — see [`installation.md`](installation.md)
and [`configuration.md`](configuration.md).

Obol enforces spend, so its state is real money state. The two things to
internalize:

1. **The state directory is the source of truth** — back it up.
2. **The WAL makes a crash safe** — a killed or crashed daemon loses no committed
   transition; recovery replays the log.

---

## Durability model (what's on disk, and why a crash is safe)

Each account is a directory under `-state-dir` with three files
([`installation.md`](installation.md#2-choose-a-state-directory-and-socket)):

- **`wal.log`** — an append-only log of every committed transition (submit, start,
  settle, top-up, transfer, reprice, set-rate/window/burst, …). This is the
  durability path **and** the audit trail.
- **`snapshot.json`** — a point-in-time capture of config + ledger, written once
  when the account is created, recording the WAL offset it covers.
- **`account.json`** — daemon-owned metadata (account name + access lists).

**How a transition commits:** the command is appended to the WAL *before* the
in-memory state changes, and durability lands via **group commit** — appends hit
the page cache and return; a background committer batches `fdatasync`s off the hot
path (with `-sync true`, the production default). The discipline: a crash before
the fsync loses the un-synced tail *and* the in-memory change together, so a torn
tail is simply discarded on replay — the books never go inconsistent.

**How recovery works:** on start, `obold` loads each account's `snapshot.json`,
then replays the WAL records after the snapshot's offset **through the same code
paths used live**, and asserts conservation. Because transitions are pure
functions of `(state, command, now)` — `now` is logged, never read from the clock
during replay — replay is deterministic and reproduces the exact pre-crash state.

> **Known characteristic:** the WAL is **never truncated** (it's the full audit
> trail), and there is currently **no periodic re-snapshot** — so recovery replays
> the entire WAL from offset 0. This is fast at normal scale and keeps a complete
> transaction history, but the WAL grows over the account's life. Log-compaction /
> periodic snapshotting is a future optimization, not a correctness issue.

---

## Backup & restore

**Backup = copy the state directory.** It's plain files; a consistent copy is all
you need.

- **Cold (simplest):** stop `obold` (clean shutdown flushes the WAL), copy
  `-state-dir`, restart.
  ```
  systemctl stop obold
  tar czf obol-state-$(date +%F).tgz -C /var/lib obol
  systemctl start obold
  ```
- **Hot:** the WAL is append-only and the daemon never rewrites history, so a
  copy taken while running is safe to *restore* — you get a consistent state as of
  some point at-or-before the copy (a torn WAL tail is discarded on replay, exactly
  as after a crash). Prefer a filesystem snapshot (LVM/ZFS/`cp --reflink`) for a
  crisp instant. Cold backup is still the recommendation for archival.

**Restore:** stop `obold`, replace `-state-dir` with the backup, start. Recovery
replays each WAL onto its snapshot; `obol list` / `obol show` confirm balances.

**Retention / audit export:** `obol log --account <name>` renders that account's
full WAL as a time-ordered transaction log (submits, settlements, top-ups,
transfers, config changes — each with amounts and the logical timestamp). Capture
it for audit/export:

```
obol --socket /run/obol/obold.sock log --account lab_smith > lab_smith-ledger.txt
```

Since the WAL isn't truncated, the log is the complete history for the account's
lifetime.

---

## Recovery after failure

| Failure | What happens | What you do |
|---------|--------------|-------------|
| **`obold` crash / kill / OOM** | committed transitions are durable in the WAL; the in-flight (un-fsynced) tail, if any, is discarded | just restart — recovery replays the WAL, asserts conservation, resumes. No data loss of committed state |
| **Clean shutdown** (SIGTERM/SIGINT) | daemon closes the listener and flushes the WAL; **no snapshot is written on exit** (the WAL is the record) | restart normally |
| **Slurm controller restart / failover** | `obold` state is independent of slurmctld | ensure `obold` is up **before** slurmctld takes submits (`Before=slurmctld` in the unit); then run a reconcile (below) to catch any jobs that ended while the gate history was out of sync |
| **Network/socket blip at submit** | the shim can't reach `obold` → the shim applies its **local fail policy** (below) | nothing; transient. Reconcile later if in doubt |
| **State-dir loss** | budgets are gone | restore from backup |
| **Conservation assertion fails on replay** | treated as **corruption**, not a warning — `obold` refuses to start that account | restore that account's dir from backup; file a bug (this should never happen) |

The design behind all of this is in [`SEAM_DESIGN.md`](SEAM_DESIGN.md) §3 (latency
tiers / group commit) and its recovery notes.

---

## Orphan reconciliation (the two janitors)

A job can leave an escrow "stuck" if its lifecycle event is lost (a completion that
never fired, a crash between phases). Obol reclaims these with two janitors that
**partition the work by whether the job ever started**, so they never race:

### 1. Unbound-token TTL sweep — automatic, in `obold`

Between the gate (escrow minted) and the job binding to a Slurm job id, a crash
could strand an escrow that was never bound. The daemon runs a periodic sweep that
**full-refunds any escrow that never started and is older than the TTL**:

- `-unbound-ttl` (default `15m`) — age past which an unbound escrow is presumed
  dead; `0` disables.
- `-sweep-interval` (default `1m`) — how often it runs.

Nothing to operate — it's on by default. It only ever touches *never-started*
escrows.

### 2. `obol reconcile` — started-orphan sweep, from a live-job feed

The complement: a **started** escrow whose job vanished (a lost completion event,
or a crash that lost the daemon's in-memory routing) can't age out via the TTL
sweep (it *did* start). `obol reconcile` reclaims these — you hand `obold` the set
of currently-live Slurm job ids and it **full-refunds started escrows not in that
set**. Run it periodically (cron/timer on the controller), and once after a
controller restart:

```
squeue -h -o '%A' | obol --socket /run/obol/obold.sock reconcile
```

It's an **admin** verb (see below). It never touches never-started escrows (those
are the TTL sweep's job) — so the two are safe to run together. `reconcile` with
an empty live set means "nothing is live," so use it deliberately.

> A suggested cadence: a systemd timer every 5–15 min piping `squeue` into
> `obol reconcile`, plus the automatic unbound sweep. Together they guarantee no
> escrow stays stuck.

---

## Fail-open vs. fail-closed (what happens if `obold` is down)

If the shim can't reach `obold` at submit, it must decide whether to admit or
reject the job **locally** — it can't ask the daemon, because the daemon being
down is the whole scenario. That decision is a **per-partition policy in the
shim** (`seam/lua/job_submit.lua`), not in the daemon:

- **fail-closed** (e.g. cloud partitions): reject the submission when `obold` is
  unreachable — no ungated spend on real-money compute.
- **fail-open** (e.g. on-prem partitions): admit it — the hardware costs the same
  regardless, so don't block science on a daemon hiccup.

Edit the `fail_closed` table in the shim to match your partitions. The default
errs toward fail-open for unlisted partitions; set cloud/chargeback partitions to
fail-closed explicitly. This is separate from the *bill-vs-write-off* policy for
node failures (see [`concepts.md` §7](concepts.md#7-partition-policy-billed-vs-written-off)).

---

## Monitoring & health

- **Liveness:** `obol --socket … ping` (exit 0 = reachable). Wire it to your
  health checker.
- **Per-account state:** `obol show --account <name>` prints balance, reserved,
  consumed, write-off, burn rate, time-to-empty, burst pot, and a **conservation
  check** (`Conservation: OK`). Alert if it ever reads anything but OK.
- **Fleet view:** `obol list` — one row per account with balance/allocation/live
  count/status; watch for accounts nearing empty or `lapsed`.
- **Daemon logs:** `obold` logs to stderr (capture via journald). It logs startup,
  shutdown, and when the unbound janitor reclaims anything.
- **What to alarm on:** conservation not OK (should be impossible → corruption);
  the daemon down (ping fails); an account unexpectedly lapsed or at zero balance.

---

## Upgrades & rollback

Obol is **pre-1.0**: minor versions (`0.y`) may change the **wire protocol** and
the **on-disk state format**; patch versions (`0.y.z`) are fixes. See
[`CHANGELOG.md`](../CHANGELOG.md) for what changed.

**Upgrade procedure:**

1. Read the CHANGELOG for the target version — note any wire/state changes.
2. Back up `-state-dir` (above).
3. Replace the `obold` and `obol` binaries and **restart obold**. Recovery reads
   the existing state.
4. If the wire protocol version changed, **update the seam** (`job_submit.lua`,
   prolog/jobcomp, and the `obol` used by the scripts) to the matching version —
   the protocol is versioned, so a mismatched shim and daemon fail loudly rather
   than misparse. Keep the daemon and the seam's `obol` on the same release.

**Rollback:** stop `obold`, restore the pre-upgrade binaries *and* the pre-upgrade
state-dir backup, restart. Because a newer minor may have written state the older
daemon can't read, **roll back the state with the binary** — don't point an old
`obold` at state a newer one wrote.

Pin the daemon and the seam to the same version; don't run a v0.13 shim against a
v0.14 daemon across a protocol bump.

---

## Who may administer

Read verbs (`show`, `list`, `log`, `resolve`, `simulate`, `dispatch`, `ping`) are
open to anyone who can reach the socket. **Mutating** verbs — `topup`, `transfer`,
`create`, `attach`/`detach`, `set-rate`, `set-window`, `set-burst`, `reconcile` —
require an **admin**:

- The check uses the connection's **kernel-verified peer identity**
  (`SO_PEERCRED`) — the uid/gid the OS reports for the socket peer, not anything on
  the wire, so it can't be spoofed.
- **root (uid 0) is always an admin.**
- Name additional admins via `admin_users` / `admin_groups` in the config
  ([`configuration.md`](configuration.md#admins)).
- **When no admin list is set, admin enforcement is off** — the socket's file
  permissions are the only boundary. On a shared controller, set an admin list
  **or** restrict who can open the socket
  ([`installation.md`](installation.md#socket-permissions--who-may-administer)).

`SO_PEERCRED` is Linux-only; on other platforms the peer-credential check fails
closed (mutating verbs are refused when identity can't be verified and enforcement
is on).

---

## Quick operational checklist

- [ ] `-state-dir` on durable storage, backed up on a schedule.
- [ ] `-sync true` (the default) in production.
- [ ] `obold` starts `Before=slurmctld`; restarts cleanly.
- [ ] `admin_users`/`admin_groups` set (or the socket locked down) on a shared host.
- [ ] Per-partition `fail_closed` set correctly in the shim (cloud → closed).
- [ ] A periodic `squeue … | obol reconcile` timer, plus the automatic unbound sweep.
- [ ] Monitoring on `ping`, `conservation`, and low/lapsed balances.
- [ ] Upgrade playbook: back up state, bump daemon + seam together, know the rollback.
