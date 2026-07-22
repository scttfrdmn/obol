# `obol` CLI reference

`obol` is the admin/diagnostic client; it talks to a running `obold` over its Unix
socket. Every command takes `--socket` (default `/run/obol/obold.sock`); it may be
given before or after the verb.

```
obol [--socket PATH] <verb> [flags]
obol help          # usage summary
```

**Exit codes** (uniform across verbs, so scripts can branch):

| Code | Meaning |
|------|---------|
| `0` | success |
| `1` | transport/daemon error (couldn't reach `obold`, malformed reply, or an operation error like a rejected mutation) |
| `2` | usage error (bad/missing flags) |
| `3` | a **clean "no"** — the gate/simulate/dispatch decision was reject/hold (distinct from `1` so a script can tell "denied" from "daemon down") |

**Admin verbs.** `create`, `attach`, `detach`, `topup`, `transfer`, `set-rate`,
`set-window`, `set-burst`, and `reconcile` mutate money or config and require an
**admin** (root, or a configured `admin_user`/`admin_group`, verified via
`SO_PEERCRED`). See [operations.md](operations.md#who-may-administer). The rest are
read-only and open to anyone who can reach the socket.

---

## Inspection

### `show` — a budget snapshot
```
obol show [--account A]
```
Balance / allocation, reserved, consumed, write-off, cost rate, time-to-empty,
window, live escrows & arrays, burst pot/ceiling, and the **conservation check**.
`--account` is required when more than one account is configured. Exit `1` if
conservation is ever violated.

### `list` — all visible accounts
```
obol list
```
One row per account the caller may see: balance, allocation, live count, status
(active/lapsed).

### `log` — an account's transaction log
```
obol log [--account A]
```
Renders the account's WAL as a time-ordered ledger (submits, settlements,
top-ups, transfers, config changes — each with amounts and the logical
timestamp). The full history for the account's lifetime (the WAL isn't truncated).

### `ping` — liveness
```
obol ping
```
Prints `ok` and exits `0` if `obold` is reachable.

---

## Dry-run / decision preview (read-only)

### `resolve` — explain the gate decision
```
obol resolve --account A [--partition P] [--time-limit S] [--uid U]
```
Without escrowing anything: which budget the account resolves to, the effective
rate and where it came from (node-type / TRES / flat), the balance, the cost (if
`--time-limit` given), whether the submitter is authorized (`--uid`), and whether
the gate would admit. Exit `3` if it would reject.

### `simulate` (alias `estimate`) — would it fund, and runway?
```
obol simulate --account A --time-limit S [--partition P] [--cpus N] [--gpus N] [--mem N]
```
The gate's money verdict for a hypothetical job — cost, balance, and **runway**
(time-to-empty at the current balance/rate) — committing nothing. Exit `0` =
would fund, `3` = would not.

### `dispatch` (alias `may-dispatch`) — burst headroom
```
obol dispatch --account A --time-limit S [--partition P] [--cpus N] [--gpus N] [--mem N]
```
Whether a pending job has the **burst headroom to start now** or must hold —
rate, reservation, projected pot, and `WOULD DISPATCH` / `WOULD HOLD (reason)`.
Exit `0` = dispatch, `3` = hold. (This is the daemon-side equivalent of the
`site_factor` plugin's per-cycle query.)

---

## Job lifecycle (used by the seam, exposed for testing)

These are what the Slurm shim/prolog/jobcomp drive; you rarely run them by hand.

### `gate` — the submit gate
```
obol gate (--account A | --source A --source B ...) --partition P --time-limit S [--ntasks N] [--uid U]
```
Escrow a job's projected cost and mint a correlation token (`allow <token>`), or
reject (exit `3`). `--ntasks N` (N>1) gates a job array. `--source` (repeatable)
funds from **multiple** accounts in ordered fallback instead of a single
`--account` (multi-source funding).

### `bind` — bind token ↔ job id (fires the start event)
```
obol bind --token T --jobid J [--node-type NT] [--idx I]
```
Bind the gate token to the Slurm job id (and dispatch it). `--node-type` triggers
the node-type reprice; `--idx I` binds one task of an array.

### `settle` — close out a job
```
obol settle (--jobid J | --token T) --kind KIND [--runtime S] [--elapsed S] [--idx I] [--if-present]
```
`KIND` is `complete | timeout | cancel | infrafail`. `--runtime` bills a clean
completion; `--elapsed` bills a cancel/infra-fail. `--idx I` settles one array
task. `--if-present` makes an already-settled/unknown job a no-op success (for
completion hooks that may double-fire).

---

## Account & money administration (admin)

### `create` — create an account at runtime
```
obol create --account A --balance N --rate R [--window D] \
            [--allow-user U]... [--allow-group G]... \
            [--burst-ceiling-pct P] [--burst-draw-cap N]
```
A new funded budget. `--burst-ceiling-pct` (0–1) turns burst on for it. Persists
to its own state dir and is rediscovered on restart.

### `attach` / `detach` — grant / revoke access
```
obol attach --account A [--user U]... [--group G]...
obol detach --account A [--user U]... [--group G]...
```
Add or remove users/groups on an account's allow-list. Reports the resulting
access (or "open" if the list becomes empty).

### `topup` — add money
```
obol topup --account A --amount N
```
Add `N` units (positive) to the balance and allocation; conservation preserved.

### `transfer` — move money between accounts
```
obol transfer --from A --to B (--amount N | --all)
```
Move `N` units (or the entire available balance with `--all`) from one account to
another, atomically and crash-safely.

### `set-rate` — change the flat cost rate
```
obol set-rate --account A --rate N
```
Future flat-rate submits bill at `N`/second; live escrows keep their frozen rate.

### `set-window` — change the time window
```
obol set-window --account A (--window D | --start T --end T)
```
`--window D` sets `[now, now+D)`; or give explicit `--start`/`--end` (RFC3339 or
epoch seconds).

### `set-burst` — change burst config
```
obol set-burst --account A (--ceiling-pct P [--draw-cap N] | --disable)
```
Enable/re-ceiling burst (`--ceiling-pct` in (0,1], optional `--draw-cap`), or turn
it off with `--disable`. A logged change that survives restart.

### `reconcile` — reclaim orphaned escrows
```
squeue -h -o '%A' | obol reconcile          # live ids from stdin
obol reconcile <jobid>...                    # or from args
```
Full-refund **started** escrows whose Slurm job id is no longer live (lost
completions, or a crash that stranded routing). Run periodically. See
[operations.md](operations.md#2-obol-reconcile--started-orphan-sweep-from-a-live-job-feed).

---

## Notes

- **`--socket` placement:** `obol --socket X show` and `obol show --socket X` are
  equivalent (a leading `--socket` is hoisted past the verb).
- This reference tracks the CLI at the current release; run `obol help` for the
  built-in summary, and see [`concepts.md`](concepts.md) for what the verbs mean.
