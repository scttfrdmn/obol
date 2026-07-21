-- job_submit.lua — obol GATE shim for Slurm's JobSubmitPlugins=lua.
--
-- Runs inside slurmctld on the controller lock at every submission. It must be
-- FAST and must never block on disk (docs/SEAM_DESIGN.md §1). It does exactly
-- one thing: read a few job_desc fields, make one loopback GATE call to obold,
-- and either stamp the returned token into admin_comment and return SUCCESS, or
-- reject with a user-facing message.
--
-- GATE only — no site_factor/burst here (that plugin is separate, v0.3.0).
--
-- Configuration (edit for the site, or set via the plugin's environment):
--   OBOL_SOCKET     obold Unix socket path (default /run/obol/obold.sock)
--   OBOL_TIMEOUT_MS hard timeout for the gate call (default 50)
--   fail_closed     per-partition table below: cloud fails closed, on-prem open
--
-- The fail_closed decision (what to do when obold is unreachable) is LOCAL and
-- static — it must not require a round-trip, because the daemon being down is
-- the whole scenario (§6/§7). Its Go model and tests live in internal/shim.

-- The Lua plugin inside slurmctld does not inherit our LUA_PATH, so add the
-- directory holding the obol modules to package.path before requiring them. The
-- dir is configurable via OBOL_LUA_DIR (default /etc/slurm/lua).
local obol_lua_dir = os.getenv("OBOL_LUA_DIR") or "/etc/slurm/lua"
package.path = obol_lua_dir .. "/?.lua;" .. package.path
-- luasocket ships C modules (socket/core.so, socket/unix.so). slurmctld's
-- embedded interpreter may not have the system cpath, so add the common lib
-- dirs explicitly. Harmless if already present or if the FFI fallback is used.
package.cpath = table.concat({
  "/usr/lib64/lua/5.4/?.so",
  "/usr/lib/lua/5.4/?.so",
  "/usr/lib64/lua/5.3/?.so",
  package.cpath,
}, ";")

local wire = require("obol_wire")
local transport = require("obol_transport")

local OBOL_SOCKET = os.getenv("OBOL_SOCKET") or "/run/obol/obold.sock"
local OBOL_TIMEOUT_MS = tonumber(os.getenv("OBOL_TIMEOUT_MS")) or 50

-- Per-partition fail-closed table. true = cloud (reject when daemon down),
-- false/absent = on-prem (admit when daemon down). Edit for the site.
local fail_closed = {
  ["cloud"] = true,
  ["cloud-spot"] = true,
  -- on-prem partitions omitted => fail open
}
local default_fail_closed = false

-- slurm.* constants are provided by the plugin host. Fall back to the documented
-- integer values so the module also loads under a plain `lua` for testing.
local SUCCESS = (slurm and slurm.SUCCESS) or 0
local ERROR = (slurm and slurm.ERROR) or -1

-- log_info wraps slurm.log_info when present (nil under plain lua).
local function log_info(fmt, ...)
  if slurm and slurm.log_info then slurm.log_info(fmt, ...) end
end

-- gate_decision performs the GATE round-trip and returns (ok, token_or_reason).
-- On transport failure it returns nil so the caller applies the fail-closed
-- policy. This function is the whole hot path.
local function gate_decision(account, partition, uid, time_limit, ntasks, tres)
  local req = wire.gate_frame({
    account = account or "",
    partition = partition or "",
    uid = uid or 0,
    time_limit = time_limit or 0,
    ntasks = ntasks or 1,
    tres = tres,
  })
  local resp, err = transport.round_trip(OBOL_SOCKET, wire.encode_frame(req), OBOL_TIMEOUT_MS)
  if not resp then
    return nil, err -- transport failure: caller applies fail-closed policy
  end
  local frame, derr = wire.decode_frame(resp)
  if not frame or not frame.gate_resp then
    return nil, derr or "malformed gate response"
  end
  return true, frame.gate_resp
end

-- slurm_job_submit is the plugin entry point. job_desc is the mutable submission
-- record; part_list and submit_uid are provided by Slurm. Returns a Slurm rc.
function slurm_job_submit(job_desc, part_list, submit_uid)
  local partition = job_desc.partition
  local account = job_desc.account
  -- job_desc.time_limit is in MINUTES and arrives as a Lua number that may be a
  -- float (e.g. 1.0). The wire fields are int64 on the Go side, so coerce to an
  -- integer count of seconds; a non-positive or sentinel (NO_VAL) limit becomes 0.
  local time_limit = tonumber(job_desc.time_limit) or 0
  local tl_seconds = 0
  if time_limit > 0 and time_limit < 0xFFFFFFFF then
    tl_seconds = math.floor(time_limit * 60)
  end
  local ntasks = 1

  -- TRES the job requested, for weighted cost (SEAM_DESIGN §5; wire TRES fields).
  -- Slurm exposes these as job_desc numbers; coerce to integers, default 0.
  local function num(v) local n = tonumber(v); return (n and n > 0 and n < 0xFFFFFFFF) and math.floor(n) or 0 end
  local tres = {
    cpus = num(job_desc.min_cpus) + 0, -- requested CPUs (min_cpus is the portable field)
    gpus = num(job_desc.gpus),         -- --gpus; GRES gpu handled site-side if needed
    mem = num(job_desc.pn_min_memory), -- per-node memory request, MB
  }

  local ok, result = gate_decision(account, partition, submit_uid, tl_seconds, ntasks, tres)

  if ok == nil then
    -- Daemon unreachable: apply the local static fail-closed policy.
    log_info("obol: gate transport failed: %s", tostring(result))
    local fc = fail_closed[partition]
    if fc == nil then fc = default_fail_closed end
    if fc then
      if slurm then slurm.log_user("obol: budget daemon unreachable; %s fails closed", partition) end
      return ERROR
    end
    log_info("obol: budget daemon unreachable; %s fails open (admit)", partition)
    return SUCCESS
  end

  if not result.allow then
    if slurm then slurm.log_user("obol: job rejected — %s", result.reason or "insufficient budget") end
    return ERROR
  end

  -- Stamp the correlation token into admin_comment (admin-controlled, so the
  -- user cannot forge it to dodge the gate — §4). Preserve any existing content.
  local tag = "budget:" .. tostring(result.token):gsub("^budget:", "")
  if job_desc.admin_comment and #job_desc.admin_comment > 0 then
    job_desc.admin_comment = job_desc.admin_comment .. " " .. tag
  else
    job_desc.admin_comment = tag
  end
  log_info("obol: gated ok, token %s", result.token)
  return SUCCESS
end

-- slurm_job_modify is required by the plugin ABI; budgets are not re-gated on
-- modify in this MVP (walltime growth is a known follow-up, tracked separately).
function slurm_job_modify(job_desc, job_rec, part_list, modify_uid)
  return SUCCESS
end

return slurm_job_submit
