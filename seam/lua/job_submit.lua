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

-- array_task_count parses a Slurm array spec string (job_desc.array_inx) into a
-- task count. Handles ranges ("0-9"), a %throttle suffix ("0-9%4" -> the %4 is a
-- concurrency limit, NOT a task count, so it's stripped), step ranges ("0-9:2"),
-- and comma lists ("1,3,5" / "0-3,7,10-12"). Returns 1 for nil/empty/unparseable
-- (fail-safe: a single escrow, never a mis-sized array). The count is what the
-- gate escrows N*c*w against.
local function array_task_count(spec)
  if type(spec) ~= "string" or spec == "" then return 1 end
  -- Drop the %throttle (concurrency cap), which doesn't change task count.
  spec = spec:gsub("%%%d+", "")
  local count = 0
  for piece in spec:gmatch("[^,]+") do
    local lo, hi, step = piece:match("^(%d+)%-(%d+):(%d+)$")
    if lo then
      lo, hi, step = tonumber(lo), tonumber(hi), tonumber(step)
      if lo and hi and step and step > 0 and hi >= lo then
        count = count + math.floor((hi - lo) / step) + 1
      end
    else
      lo, hi = piece:match("^(%d+)%-(%d+)$")
      if lo then
        lo, hi = tonumber(lo), tonumber(hi)
        if lo and hi and hi >= lo then count = count + (hi - lo) + 1 end
      elseif piece:match("^%d+$") then
        count = count + 1
      end
    end
  end
  if count < 1 then return 1 end
  return count
end

-- Exposed as a global for unit testing (Slurm only calls the slurm_job_* globals;
-- an extra global is harmless to the plugin ABI). Tests load this file and call it.
obol_array_task_count = array_task_count

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
  -- Job arrays (#103): slurm_job_submit fires ONCE for the whole array, and the
  -- array spec is job_desc.array_inx (verified on 22.05/23.11/24.05) — a string
  -- like "0-9", "0-9%4" (a %throttle), or a list "1,3,5". Count the tasks so the
  -- gate escrows the whole array (NTasks>1 -> SubmitArray). A non-array job leaves
  -- array_inx nil -> ntasks 1 (unchanged). Anything unparseable falls back to 1
  -- (fail-safe to a single escrow rather than mis-sizing the array).
  local ntasks = array_task_count(job_desc.array_inx)

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
