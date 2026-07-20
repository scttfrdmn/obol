# seam/ — the Slurm attachment

The glue between the obol daemon (`obold`) and a running Slurm controller. See
[`docs/SEAM_DESIGN.md`](../docs/SEAM_DESIGN.md) for the architecture and the "why".

This is the **GATE-only MVP seam**: enough to enforce budgets at submission and
settle on job exit. The production burst dispatch gate (`site_factor`) is not
here yet — it needs per-generation cluster validation (v0.3.0).

## Contents

| File | Role |
|------|------|
| `lua/obol_wire.lua` | Pure-Lua implementation of the obol wire framing (length prefix + IEEE crc32 + JSON), mirroring Go's `internal/wire`. No external Lua deps. |
| `lua/obol_transport.lua` | One-shot Unix-socket round-trip for the GATE call, with a hard timeout. Backends: luasocket, then LuaJIT FFI. |
| `lua/job_submit.lua` | The `JobSubmitPlugins=lua` shim: reads `job_desc`, one GATE call to obold, stamps the token into `admin_comment`, SUCCESS/reject. Applies the local fail-closed policy when obold is unreachable. |
| `slurm/obol-prolog.sh` | Prolog: reads the token from `admin_comment` and BINDs token↔jobid at job start. |
| `slurm/obol-epilog.sh` | Epilog: SETTLEs the escrow on job exit, mapping Slurm exit state to complete/timeout/cancel/infrafail. |

## Requirements

- **Lua 5.3+** on the controller (native bitwise operators and integer subtype).
  Slurm 24.05 on Rocky 9 ships Lua 5.4. ✔
- **luasocket** (`socket.unix`) for the transport, or a LuaJIT controller (FFI
  fallback). The Docker/PC integration images install luasocket.

## slurm.conf

```
JobSubmitPlugins=lua           # loads job_submit.lua from the plugin dir
Prolog=/path/to/obol-prolog.sh
Epilog=/path/to/obol-epilog.sh
```

Environment for the scripts and shim:

```
OBOL_SOCKET=/run/obol/obold.sock   # must match obold -socket
OBOL_BIN=/usr/local/bin/obol       # epilog/prolog CLI path
OBOL_TIMEOUT_MS=50                 # shim hard timeout for the GATE call
```

## Testing

`go test ./seam/lua/` cross-validates the Lua wire module against the Go
reference (`internal/wire`) in both directions and checks the crc matches Go's
`crc32.IEEE`. It skips when no `lua` interpreter is on PATH. The full
shim→socket→obold path is exercised in the Docker single-node Slurm tier
(`make integ-docker`, issue #34), which installs luasocket.
