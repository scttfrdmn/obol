# Installing Obol

Obol is two binaries and a Slurm shim:

- **`obold`** — the sidecar daemon that holds the budgets and answers the gate.
- **`obol`** — the admin/diagnostic CLI that talks to `obold` over its socket.
- the **seam** — `job_submit.lua` + prolog/jobcomp scripts installed into Slurm so
  the controller calls `obold` at submit/start/completion.

This guide covers getting the binaries onto the controller host and running the
daemon. For *what to put in the config*, see [`configuration.md`](configuration.md);
for wiring the Slurm side, see [`INTEGRATION.md`](INTEGRATION.md) and
[`../seam/README.md`](../seam/README.md).

> **Where it runs.** `obold` runs on the **Slurm controller host** (next to
> slurmctld), because the `job_submit` plugin calls it on the controller over a
> local Unix socket. It is a single process; there is no clustering.

---

## 1. Get the binaries

### Release binary (recommended)

Download the archive for your OS/arch from the
[releases page](https://github.com/scttfrdmn/obol/releases) and extract `obold`
and `obol`. Releases are built by GoReleaser for linux `amd64`/`arm64`; the
version is stamped in (`obold version`).

### From source

Requires **Go 1.26** (the single supported toolchain).

```
git clone https://github.com/scttfrdmn/obol
cd obol
make build        # -> bin/obold, bin/obol  (version stamped via -ldflags)
```

Verify:

```
bin/obold version      # prints: obold <version>
```

Install them somewhere on `PATH` for the controller and the seam scripts to find
(the scripts default to `/usr/local/bin/obol`):

```
sudo install -m 0755 bin/obold bin/obol /usr/local/bin/
```

---

## 2. Choose a state directory and socket

`obold` needs two paths, both with sensible defaults:

| Path | Flag | Default | What it is |
|------|------|---------|------------|
| state dir | `-state-dir` | `/var/lib/obol` | per-account WAL + snapshot (the durable money state) |
| socket | `-socket` | `/run/obol/obold.sock` | the local Unix socket the shim and CLI connect to |

The state directory is **the source of truth for every budget** — back it up, and
don't put it on volatile/tmpfs storage. Its layout is one subdirectory per
account:

```
/var/lib/obol/
  lab_smith/
    snapshot.json     # periodic state snapshot (config + ledger)
    wal.log           # append-only command log replayed on recovery
    account.json      # daemon-owned: account name + access lists
  lab_jones/
    ...
  transfers/          # in-flight obol-transfer journal records (normally empty)
```

`obold` creates the socket's parent directory (`0750`) if missing and clears a
stale socket from an unclean prior exit on start.

---

## 3. Run the daemon

Multi-account (the normal deployment) — point it at a config file
([`configuration.md`](configuration.md)):

```
obold -socket /run/obol/obold.sock -state-dir /var/lib/obol -config /etc/obol/obold.json
```

Single-budget (quick trials / one flat pot, no config file) — the `-create` flag
bootstraps one account named `default` from the flags:

```
obold -state-dir /var/lib/obol -create -balance 100000 -rate 1 -window 720h
```

On start `obold` **discovers** any existing account directories under `-state-dir`
and recovers them (replaying each WAL onto its snapshot), then creates any config
accounts not already on disk. So a restart is safe and idempotent — on-disk state
wins, the config only fills gaps.

### `obold` flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-socket` | `/run/obol/obold.sock` | Unix listen socket |
| `-state-dir` | `/var/lib/obol` | per-account WAL + snapshot directory |
| `-config` | *(none)* | multi-account config JSON; omit to use the single-budget flags below |
| `-sync` | `true` | `fdatasync` the WAL on every append. **Leave true in production** — false trades durability for throughput (tests only) |
| `-unbound-ttl` | `15m` | reclaim escrows minted at the gate but never bound to a job id (submit→start orphans); `0` disables the sweep |
| `-sweep-interval` | `1m` | how often the unbound-token janitor runs |
| **single-budget bootstrap** (ignored when `-config` is given): | | |
| `-create` | `false` | create a fresh `default` budget if none exists in `-state-dir` |
| `-balance` | `0` | initial allocation for the created budget |
| `-rate` | `1` | flat cost per second (units/sec) for the created budget |
| `-window` | `720h` | window length for the created budget |
| `-tres-per-cpu` / `-tres-per-gpu` / `-tres-per-mem` | `0` | cost per allocated CPU / GPU / MB-second (0 = flat rate) |

`-config` and the single-budget bootstrap flags are mutually exclusive: with
`-config`, the `-create`/`-rate`/`-balance`/`-window`/`-tres-*` flags are ignored
(accounts and pricing come from the file).

---

## 4. Run it as a service

`obold` runs in the foreground and exits cleanly on `SIGINT`/`SIGTERM` (closing
the listener and flushing the WAL). A minimal systemd unit on the controller:

```ini
# /etc/systemd/system/obold.service
[Unit]
Description=obol budget daemon
After=network.target
Before=slurmctld.service          # gate must be up before the controller takes submits

[Service]
ExecStart=/usr/local/bin/obold -socket /run/obol/obold.sock -state-dir /var/lib/obol -config /etc/obol/obold.json
Restart=on-failure
RestartSec=2s
# obold creates /run/obol itself; RuntimeDirectory keeps it across restarts:
RuntimeDirectory=obol
StateDirectory=obol               # manages /var/lib/obol ownership
User=slurm
Group=slurm

[Install]
WantedBy=multi-user.target
```

```
sudo systemctl daemon-reload
sudo systemctl enable --now obold
```

A restart is safe at any time — recovery replays the WAL, so no state is lost by a
kill/crash. See [`operations.md`](operations.md) for backup, upgrade, and recovery
procedures.

### Socket permissions & who may administer

The socket is how both the shim (slurmctld) and admins reach `obold`. Its access
is governed by the socket's **filesystem permissions** — whoever can open
`/run/obol/obold.sock` can send read verbs. **Mutating** verbs (top-up, transfer,
create, set-rate/window/burst, reconcile) are additionally gated on the
connection's **kernel-verified peer identity** (`SO_PEERCRED`): root is always an
admin, and you can name `admin_users`/`admin_groups` in the config
([`configuration.md`](configuration.md#admins)). When no admin list is set, admin
enforcement is off and the socket's file permissions are the only boundary — so on
a shared controller, restrict the socket directory or set an admin list.

---

## 5. Wire up Slurm

Install the seam so slurmctld calls Obol. In `slurm.conf`:

```
JobSubmitPlugins=lua                       # with seam/lua/job_submit.lua in the plugin dir
Prolog=/usr/local/bin/obol-prolog.sh       # binds token↔jobid at start
PrologFlags=Alloc
JobCompType=jobcomp/script                 # settlement feed
JobCompLoc=/usr/local/bin/obol-jobcomp.sh
```

The Lua modules (`obol_wire.lua`, `obol_transport.lua`, `job_submit.lua`) and the
prolog/jobcomp scripts live in [`../seam/`](../seam/); the environment they read
(`OBOL_SOCKET`, `OBOL_BIN`, …) and the exact placement are documented in
[`../seam/README.md`](../seam/README.md). The containerized tier in
[`../test/docker/`](../test/docker/) is a complete, working reference install.

---

## 6. Verify

`obol` uses the default socket (`/run/obol/obold.sock`) automatically — pass
`--socket PATH` only if you ran `obold -socket` on a non-default path.

```
obol ping                        # daemon reachable
obol list                        # accounts + balances
obol show --account lab_smith
sbatch --account=lab_smith --time=1 --wrap="true"   # gated at submit
```

If `list` shows your accounts with the balances from the config, the daemon and
config are good; if `sbatch` is gated (reserved balance visible in `show`, or a
rejection for an unfunded job), the seam is wired correctly.
