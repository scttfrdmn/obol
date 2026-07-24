# seam/ — the Slurm attachment

The glue between the obol daemon (`obold`) and a running Slurm controller. See
[`docs/SEAM_DESIGN.md`](../docs/SEAM_DESIGN.md) for the architecture and the "why".

This is the **GATE seam** plus a **reference burst dispatch plugin**: the Lua +
shell pieces enforce budgets at submission and settle on job exit (tested in the
Docker tier), and `plugin/obol_site_factor.c` is documented reference source for
the burst dispatch gate. The dispatch *decision* itself is fully implemented and
tested in the daemon (`obol dispatch` / `handleDispatch`); the C plugin is the
thin Slurm-side caller, compiled per site against its own Slurm source.

## Contents

| File | Role |
|------|------|
| `lua/obol_wire.lua` | Pure-Lua implementation of the obol wire framing (length prefix + IEEE crc32 + JSON), mirroring Go's `internal/wire`. No external Lua deps. |
| `lua/obol_transport.lua` | One-shot Unix-socket round-trip for the GATE call, with a hard timeout. Backends: luasocket, then LuaJIT FFI. |
| `lua/job_submit.lua` | The `JobSubmitPlugins=lua` shim: reads `job_desc`, one GATE call to obold, stamps the token into `admin_comment`, SUCCESS/reject. When no in-process socket backend is available it falls back to exec'ing `obol gate` (#137); applies the local fail-closed policy only when both the daemon is unreachable *and* the fallback can't run. |
| `slurm/obol-prolog.sh` | Prolog: reads the token from `admin_comment` and BINDs token↔jobid at job start. |
| `slurm/obol-jobcomp.sh` | **jobcomp/script feed (primary settlement):** runs on the controller at every completion and SETTLEs the escrow, mapping Slurm state → complete/timeout/cancel/infrafail. Fires even on node failure (unlike the epilog). |
| `slurm/obol-epilog.sh` | Epilog: an optional compute-node SETTLE fallback (redundant with jobcomp; uses `settle --if-present` so a double-fire is a no-op). |
| `plugin/obol_site_factor.c` | **Reference** `site_factor` burst dispatch plugin (C — the hook has no Lua binding). Reads the token, asks obold `DISPATCH`, holds at priority 0 when there's no burst headroom. Not built/tested in CI; see `plugin/README.md`. |

## Requirements

- **Lua 5.3+** on the controller (native bitwise operators and integer subtype).
  Slurm 24.05 on Rocky 9 ships Lua 5.4. ✔
- **luasocket** (`socket.unix`) for the transport, or a LuaJIT controller (FFI
  fallback). The Docker/PC integration images install luasocket. If **neither** is
  present, the shim falls back to exec'ing the `obol` CLI for the GATE (#137) — so
  luasocket is a performance choice on the controller, not a hard requirement
  (though on a high-throughput controller you want an in-process backend, since the
  fallback forks per submit; see [`../docs/SEAM_DESIGN.md`](../docs/SEAM_DESIGN.md) §1.1).

## slurm.conf

```
JobSubmitPlugins=lua                       # loads job_submit.lua from the plugin dir
Prolog=/path/to/obol-prolog.sh             # BIND token<->jobid at start
JobCompType=jobcomp/script                 # settlement feed (primary)
JobCompLoc=/path/to/obol-jobcomp.sh
# Epilog=/path/to/obol-epilog.sh           # optional redundant fallback
```

Environment for the scripts and shim:

```
OBOL_SOCKET=/run/obol/obold.sock   # must match obold -socket
OBOL_BIN=/usr/local/bin/obol       # CLI path for the scripts
OBOL_TIMEOUT_MS=50                 # shim hard timeout for the GATE call
OBOL_LUA_DIR=/etc/slurm/lua        # where the obol_wire/obol_transport modules live
OBOL_LUA_CPATH=                    # extra ";"-separated C-module patterns, prepended
                                   # to the shim cpath (for luasocket in a nonstandard
                                   # location); the shim already searches the common
                                   # /usr/lib64, /usr/lib, and /usr/local/lib dirs
OBOL_SHELLOUT=1                    # 1 (default): fall back to exec'ing `obol gate`
                                   # when no in-process socket backend loads (#137);
                                   # 0 disables the fallback (then a missing backend
                                   # => fail-closed policy applies)
OBOL_ADDR=                         # host:port of a remote obold TCP listener (#144),
                                   # for off-host seams (e.g. PCS). Empty = the local
                                   # Unix socket. When set, the CLI (and thus the
                                   # shell-out GATE) reaches obold over TCP.
OBOL_AUTH_TOKEN=                   # bearer token for OBOL_ADDR, or
OBOL_AUTH_TOKEN_FILE=              # a file to read it from (must match obold's
                                   # -auth-token-file). Required when OBOL_ADDR is set.
```

**jobcomp runs with a minimal environment.** slurmctld invokes the `jobcomp/script`
program without inheriting `OBOL_*` or a full `PATH`, so `obol-jobcomp.sh` defaults
`OBOL` to the absolute `/usr/local/bin/obol` and `OBOL_SOCKET` to the standard path.
Install the CLI there (or export the vars into slurmctld's environment).

## Testing

`go test ./seam/lua/` cross-validates the Lua wire module against the Go
reference (`internal/wire`) in both directions and checks the crc matches Go's
`crc32.IEEE`. It skips when no `lua` interpreter is on PATH. The full
shim→socket→obold path is exercised in the Docker single-node Slurm tier
(`make integ-docker`, issue #34), which installs luasocket.
