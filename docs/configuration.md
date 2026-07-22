# Configuring Obol

`obold -config /etc/obol/obold.json` loads a multi-account configuration: the
accounts, their money balances and cost rates, optional pricing, access lists, and
who may administer. This is the reference for that file.

New to the model (balances, reservations, rates, windows, burst)? Read
[`concepts.md`](concepts.md) first — this page assumes those terms.

> **Strict parsing.** The config is parsed with unknown-field rejection: a typo'd
> or misplaced key is a **hard error at startup**, not silently ignored. The
> daemon also validates the whole file before serving (see
> [Validation](#validation)) — a bad config fails fast rather than half-loading.

---

## A minimal config

```json
{
  "accounts": [
    {"name": "lab_smith", "balance": 100000, "rate": 1, "window": "720h"},
    {"name": "lab_jones", "balance": 50000,  "rate": 1, "window": "720h"}
  ]
}
```

Two accounts, each a flat pot: `lab_smith` funded 100000 units, billed 1 unit per
second of walltime, over a 30-day window. That's a complete, valid config.

---

## Top-level fields

| Key | Type | Required | Meaning |
|-----|------|----------|---------|
| `accounts` | array | **yes** (≥1) | the budgets — see [Accounts](#accounts) |
| `admin_users` | `[string]` | no | usernames allowed to run mutating verbs — see [Admins](#admins) |
| `admin_groups` | `[string]` | no | groups allowed to run mutating verbs |
| `node_types` | `{name: rate}` | no | per-node-type pricing — see [Node-type pricing](#node-type-pricing) |
| `partitions` | array | no | which node types a partition can place on |

---

## Accounts

Each entry in `accounts` is one budget. A submission's Slurm `--account` resolves
to the entry with the **exact same `name`** (no roll-up to parents — see
[`concepts.md` §4](concepts.md#4-hierarchy--resolution-account--budget)).

| Key | Type | Default | Meaning |
|-----|------|---------|---------|
| `name` | string | *(required)* | the Slurm account name; the resolution key. Must be unique |
| `balance` | int | `0` | initial allocation in **integer units** (`B0`). Must be ≥ 0 |
| `rate` | int | *(required-ish)* | flat cost per **second** of walltime, in units. Must be > 0. Overridden per-job by TRES/node-type pricing when configured |
| `window` | string | `720h` | budget window as a [Go duration](https://pkg.go.dev/time#ParseDuration) (e.g. `720h` = 30d, `1000h`). Must be positive |
| `allow_users` | `[string]` | *(open)* | if set, only these users may fund jobs from this account — see [Access](#access) |
| `allow_groups` | `[string]` | *(open)* | likewise, by group |
| `burst_enabled` | bool | `false` | turn on banked burst for this account |
| `burst_ceiling_pct` | float | `0` | burst pot ceiling as a fraction of `balance`, in `(0, 1]`; required when `burst_enabled` |
| `burst_draw_cap` | int | `0` | max burst tokens one job may reserve; `0` = unlimited |

**On money units:** `balance` and `rate` are integers because Obol stores money
exactly (see [`concepts.md` §1](concepts.md#why-money-is-integer-units)). Choose
what a unit is worth and use it consistently — e.g. 1 unit = $0.01 for cent
precision.

### Access

By default an account is **open**: Obol trusts that if Slurm let the user submit
under this account (slurmdbd already enforces membership), it should fund the job —
so the common path incurs no extra identity lookup. Set `allow_users` and/or
`allow_groups` to *additionally* restrict funding to a named set; the check uses
the connection's kernel-verified uid, resolved to user + groups, not a spoofable
field. Example:

```json
{"name": "lab_jones", "balance": 50000, "rate": 1, "window": "720h",
 "allow_users": ["carol", "dave"]}
```

### Burst

Burst is opt-in per account and is **permission, not money**
([`concepts.md` §6](concepts.md#6-banked-burst-permission-not-money)):

```json
{"name": "burstlab", "balance": 100000, "rate": 1, "window": "720h",
 "burst_enabled": true, "burst_ceiling_pct": 0.5, "burst_draw_cap": 2000}
```

Setting `burst_ceiling_pct`/`burst_draw_cap` without `burst_enabled: true` is a
config error (so you can't accidentally think burst is on). Burst can also be
changed at runtime with `obol set-burst` (a logged change that survives restart)
and enabled on a runtime-created account with `obol create --burst-ceiling-pct`.

---

## Node-type pricing

For clusters where a partition places on mixed hardware, price per **node type**
instead of a flat rate. Two fields work together:

- `node_types` — a map from a node-type name to its rate.
- `partitions` — for each partition, the node types it can place on.

```json
{
  "node_types": {
    "spr": {"rate": 10},
    "icx": {"rate": 6},
    "a100": {"rate": 7200, "per": "h"}
  },
  "partitions": [
    {"name": "priced", "node_types": ["spr", "icx"]},
    {"name": "gpu",    "node_types": ["a100"]}
  ],
  "accounts": [ ... ]
}
```

A `node_types` entry is a **NodeRate**:

| Key | Type | Default | Meaning |
|-----|------|---------|---------|
| `rate` | int | *(required)* | cost per unit of time, must be > 0 |
| `per` | string | `s` | the time unit: `s`, `m`, or `h` |

`per` is admin convenience: `{"rate": 7200, "per": "h"}` means 7200 units/hour,
which the kernel stores as **2 units/second** (the kernel bills per second in
integer money). The rate must divide **evenly** into a whole number of
units/second, or the config is **rejected at startup** — this is what keeps money
exact:

- ✅ `{"rate": 7200, "per": "h"}` → `7200 / 3600 = 2` units/sec.
- ✅ `{"rate": 60, "per": "m"}` → `60 / 60 = 1` unit/sec.
- ❌ `{"rate": 250, "per": "h"}` → `250 / 3600` is not a whole number → rejected
  at startup: `rate 250 per "h" is not a whole number of units/second (3600 does
  not divide 250)`.

**Practical note:** because of this, choose your money-unit precision so hourly
prices land on whole units/second. Finer precision makes more rates
expressible — e.g. at **1 unit = $0.01**, a `$1.00/hour` node is `{"rate": 100,
"per": "h"}` → `100/3600`, still **not** divisible; but `$36.00/hour` is
`{"rate": 3600, "per": "h"}` → exactly `1` unit/sec. When an hourly price won't
divide, express the rate **per second** directly (`per: "s"`, the default) at a
unit precision fine enough to represent it.

How pricing is chosen at the gate (see
[`concepts.md` §3](concepts.md#3-cost-flat-rate-tres-weights-or-node-type-pricing)):
**node-type worst-case** (if the job's partition is in `partitions`, reserve the
most expensive node it could land on, then reprice down when Slurm binds a node) →
**TRES weights** (the `obold -tres-*` flags, if set) → the account's **flat rate**.

---

## Admins

Mutating verbs — `topup`, `transfer`, `create`, `attach`/`detach`, `set-rate`,
`set-window`, `set-burst`, `reconcile` — change money or config, so they require
an **admin**. Read verbs (`show`, `list`, `log`, `resolve`, `simulate`,
`dispatch`) do not.

```json
{"admin_users": ["root", "hpcadmin"], "admin_groups": ["hpc-ops"], "accounts": [ ... ]}
```

- **root (uid 0) is always an admin**, listed or not.
- The check uses the connection's kernel-verified peer identity (`SO_PEERCRED`),
  not a wire field — it can't be spoofed.
- **When both lists are empty, admin enforcement is off**: the socket's file
  permissions are the only boundary (preserving the simple single-admin case). On
  a shared controller, either set an admin list or lock down the socket
  directory — see [`installation.md`](installation.md#socket-permissions--who-may-administer).

---

## Validation

`obold` validates the entire config before it serves anything; a failure is fatal
at startup with a specific message. The rules:

- at least one account; account `name`s non-empty and unique;
- `balance` ≥ 0; `rate` > 0; `window` parseable and positive;
- burst: if `burst_enabled`, `burst_ceiling_pct` ∈ `(0, 1]` and `burst_draw_cap`
  ≥ 0; if not enabled, neither burst field may be set;
- each `node_types` rate must be a whole number of units/second (divides by its
  `per`); every partition names ≥1 node type, each of which must be a defined
  `node_type`;
- unknown/misspelled keys anywhere are rejected.

You can dry-run a submission's resolution without touching money using
`obol resolve --account A [--partition P]` (which budget, effective rate, rate
source, whether it would admit) — handy for checking pricing config.

---

## Changing config at runtime

Most of this file can also be changed live, without editing it and restarting,
via admin CLI verbs — these are **logged** transitions that survive recovery, so
they don't drift from the file:

| Change | Verb |
|--------|------|
| add an account | `obol create --account … --balance … --rate … [--window …] [--burst-ceiling-pct …]` |
| grant/revoke access | `obol attach` / `obol detach --account … --user … --group …` |
| add money | `obol topup --account … --amount …` |
| move money | `obol transfer --from … --to … (--amount … \| --all)` |
| change the flat rate | `obol set-rate --account … --rate …` |
| change the window | `obol set-window --account … (--window D \| --start T --end T)` |
| change burst | `obol set-burst --account … (--ceiling-pct P [--draw-cap N] \| --disable)` |

A runtime-created account persists to its own state dir and is rediscovered on
restart, so the config file and live state stay reconciled (on-disk wins).
