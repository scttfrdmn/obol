# seam/plugin/ — reference `site_factor` burst dispatch plugin

> **⚠️ Reference source only.** `obol_site_factor.c` is documented, compilable
> reference — it is **not built, not tested in CI, and not wired into the
> Slurm-22.05 Docker integration tier** (that tier installs Slurm from RPM and
> has no plugin headers). The **tested** equivalent of the dispatch decision is
> the Go daemon handler `handleDispatch` (`internal/daemon/dispatch_test.go`) and
> the `obol dispatch` CLI verb (`internal/cli`). Compile this against your own
> Slurm source tree; treat it as the contract-faithful skeleton, not shipped code.

## Why a C plugin (and why it's separate from the Lua seam)

The rest of the seam is Lua + shell: `JobSubmitPlugins=lua` runs the GATE shim,
and prolog/jobcomp/epilog scripts do bind/settle. But Slurm's **`site_factor`**
hook — the per-cycle priority adjustment — has **no Lua binding**; the
`priority/multifactor` plugin `dlopen`s a `.so`. So the burst dispatch gate must
be C, and it cannot reuse the Lua transport (`seam/lua/obol_transport.lua`). It
speaks the same wire framing directly (see below).

## What it does (docs/SEAM_DESIGN.md §4)

For each **pending** job, every scheduling cycle:

1. Read the correlation token obol stamped into `admin_comment`
   (`budget:<hex>`, set by `seam/lua/job_submit.lua`). No token → not an obol
   job → leave priority unchanged.
2. Ask obold over its Unix socket, via a `DISPATCH` request, "may this job start
   now?" — the daemon answers lock-free from the burst headroom
   (`Budget.MayDispatch`).
3. `dispatch=false` → set the site factor to **0** (hold at priority 0).
   `dispatch=true`, or **any error / missing token → fail-open** (leave priority
   unchanged).

**Fail-open is deliberate.** If obold is unreachable, holding every job would
freeze all scheduling. The submit-time GATE is the hard budget boundary; the
dispatch gate only shapes concurrency, so degrading to "don't hold" is safe. This
matches the prolog's daemon-down posture.

The token↔jobid **bind** and the burst **reservation** at pending→running are
already handled by the prolog → `BIND` → `bd.Start` path
(`seam/slurm/obol-prolog.sh`), so this plugin's only job is the dispatch hold.

## Wire framing (mirrors `internal/wire/wire.go`)

```
[u32 len LE][u32 crc32(IEEE) LE][JSON payload]

request : {"v":1,"k":"dispatch","dispatch":{"account":"lab","partition":"p","time_limit":3600}}
response: {"v":1,"k":"dispatch","dispatch_resp":{"ok":true,"dispatch":false,"hold":"insufficient burst headroom","reserve":500,"pot":100}}
```

`ProtocolVersion` is `1`. The daemon rejects a frame whose `"v"` it does not
understand. `crc32` is the IEEE polynomial (zlib `crc32()` / Go `crc32.IEEE`).

## Build

```sh
gcc -shared -fPIC -I<slurm-src>/src/common -I<slurm-src> \
    -o obol_site_factor.so obol_site_factor.c -lz
```

(`-lz` provides `crc32`; or vendor a CRC-32/IEEE table.)

## slurm.conf

```
PriorityType=priority/multifactor
PrioritySiteFactorPlugin=site_factor/obol   # install the .so per your Slurm's plugin dir
```

Environment: `OBOL_SOCKET` (default `/run/obol/obold.sock`).

## Adapting to your Slurm version

The `site_factor` plugin ABI (init/fini symbols, `site_factor_p_set`, the
`job_record` field names, the hold mechanism) varies across Slurm majors. The
callback body in `obol_site_factor.c` is written to the documented contract and
marked where a site must adapt. The transport/framing helpers
(`obol_write_frame`/`obol_read_frame`/`obol_dial`/`extract_token`/
`obol_may_dispatch`) are version-independent and reusable as-is.

## Verifying the decision without the plugin

Because the plugin isn't in CI, exercise the exact same daemon decision from the
CLI:

```sh
obol dispatch --account <acct> --time-limit <secs> [--partition <p>]
# -> "Verdict: WOULD DISPATCH" (exit 0) or "WOULD HOLD (<reason>)" (exit 3)
```

That query hits `handleDispatch` → `Budget.MayDispatch` — identical to what the
plugin asks.
