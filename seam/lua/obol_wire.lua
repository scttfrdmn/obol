-- obol_wire.lua — pure-Lua implementation of the obol wire framing, mirroring
-- Go's internal/wire. The job_submit shim speaks this directly so it needs no
-- external Lua dependency for framing (the socket transport is separate).
--
-- Frame: [u32 len LE][u32 crc32 LE][JSON payload]. crc32 is IEEE, matching Go's
-- hash/crc32 (crc32.IEEE), so a frame this module writes is accepted verbatim by
-- obold's ReadFrame and vice versa.
--
-- Requires Lua 5.3+ (native bitwise operators). Slurm 24.05 on Rocky 9 ships
-- Lua 5.4, which satisfies this. No external modules.

local M = { PROTOCOL_VERSION = 1 }

--------------------------------------------------------------------------------
-- crc32 (IEEE, reflected) — table-driven, matches Go crc32.ChecksumIEEE.
--------------------------------------------------------------------------------

local crc_table = {}
do
  for i = 0, 255 do
    local c = i
    for _ = 1, 8 do
      if (c & 1) ~= 0 then
        c = 0xEDB88320 ~ (c >> 1)
      else
        c = c >> 1
      end
    end
    crc_table[i] = c
  end
end

-- crc32 returns the IEEE CRC-32 of a byte string as an unsigned 32-bit number.
function M.crc32(s)
  local crc = 0xFFFFFFFF
  for i = 1, #s do
    local b = string.byte(s, i)
    crc = (crc >> 8) ~ crc_table[(crc ~ b) & 0xFF]
  end
  return (crc ~ 0xFFFFFFFF) & 0xFFFFFFFF
end

--------------------------------------------------------------------------------
-- Little-endian u32 pack/unpack.
--------------------------------------------------------------------------------

local function u32le(n)
  n = n & 0xFFFFFFFF
  return string.char(n & 0xFF, (n >> 8) & 0xFF, (n >> 16) & 0xFF, (n >> 24) & 0xFF)
end

local function read_u32le(s, off)
  local a, b, c, d = string.byte(s, off, off + 3)
  return a + b * 256 + c * 65536 + d * 16777216
end

--------------------------------------------------------------------------------
-- Minimal JSON encode/decode for our frame shapes (objects, arrays, strings,
-- numbers, booleans, null). Not a general JSON library — just enough for the
-- protocol, matching what Go's encoding/json produces and accepts.
--------------------------------------------------------------------------------

local json = {}

local esc_map = {
  ['"'] = '\\"', ['\\'] = '\\\\', ['\b'] = '\\b', ['\f'] = '\\f',
  ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t',
}

local function esc_str(s)
  return '"' .. s:gsub('[%z\1-\31"\\]', function(ch)
    return esc_map[ch] or string.format('\\u%04x', string.byte(ch))
  end) .. '"'
end

-- json.encode serializes a Lua value. Tables tagged with __array are arrays;
-- otherwise a table is an object. Integer numbers are emitted without a decimal
-- point (Go decodes them into int64 fields cleanly).
function json.encode(v)
  local t = type(v)
  if t == "nil" then
    return "null"
  elseif t == "boolean" then
    return v and "true" or "false"
  elseif t == "number" then
    -- Emit whole numbers without a decimal point so Go decodes them into int64
    -- fields cleanly — including whole-valued FLOATS like 60.0 (Slurm passes
    -- job_desc numbers as floats). Go's json rejects "60.0" into an int64, so a
    -- float that is integral must still serialize as "60".
    if v == math.floor(v) and v < 2 ^ 53 and v > -(2 ^ 53) then
      return string.format("%d", v)
    end
    return tostring(v)
  elseif t == "string" then
    return esc_str(v)
  elseif t == "table" then
    if v.__array then
      local parts = {}
      for i = 1, #v do parts[i] = json.encode(v[i]) end
      return "[" .. table.concat(parts, ",") .. "]"
    end
    local parts = {}
    -- Deterministic key order helps tests; order does not matter to Go.
    local keys = {}
    for k in pairs(v) do
      if k ~= "__array" then keys[#keys + 1] = k end
    end
    table.sort(keys)
    for _, k in ipairs(keys) do
      parts[#parts + 1] = esc_str(k) .. ":" .. json.encode(v[k])
    end
    return "{" .. table.concat(parts, ",") .. "}"
  end
  error("json.encode: unsupported type " .. t)
end

-- json.decode parses a JSON string into Lua values. Objects become tables;
-- arrays become tables with sequential integer keys. Sufficient for responses.
function json.decode(s)
  local pos = 1

  local function skip_ws()
    local _, e = s:find("^[ \t\r\n]*", pos)
    pos = e + 1
  end

  local parse_value

  local function parse_string()
    pos = pos + 1 -- opening quote
    local buf = {}
    while pos <= #s do
      local ch = s:sub(pos, pos)
      if ch == '"' then
        pos = pos + 1
        return table.concat(buf)
      elseif ch == "\\" then
        local nxt = s:sub(pos + 1, pos + 1)
        local m = { ['"'] = '"', ['\\'] = '\\', ['/'] = '/', b = '\b',
                    f = '\f', n = '\n', r = '\r', t = '\t' }
        if m[nxt] then
          buf[#buf + 1] = m[nxt]
          pos = pos + 2
        elseif nxt == "u" then
          local hex = s:sub(pos + 2, pos + 5)
          buf[#buf + 1] = utf8.char(tonumber(hex, 16))
          pos = pos + 6
        else
          error("json.decode: bad escape \\" .. nxt)
        end
      else
        buf[#buf + 1] = ch
        pos = pos + 1
      end
    end
    error("json.decode: unterminated string")
  end

  local function parse_number()
    local _, e = s:find("^-?%d+%.?%d*[eE]?[+-]?%d*", pos)
    local num = s:sub(pos, e)
    pos = e + 1
    return tonumber(num)
  end

  local function parse_object()
    pos = pos + 1 -- {
    local obj = {}
    skip_ws()
    if s:sub(pos, pos) == "}" then pos = pos + 1; return obj end
    while true do
      skip_ws()
      local key = parse_string()
      skip_ws()
      pos = pos + 1 -- :
      obj[key] = parse_value()
      skip_ws()
      local ch = s:sub(pos, pos)
      pos = pos + 1
      if ch == "}" then return obj end
      if ch ~= "," then error("json.decode: expected , or } got " .. ch) end
    end
  end

  local function parse_array()
    pos = pos + 1 -- [
    local arr = {}
    skip_ws()
    if s:sub(pos, pos) == "]" then pos = pos + 1; return arr end
    while true do
      arr[#arr + 1] = parse_value()
      skip_ws()
      local ch = s:sub(pos, pos)
      pos = pos + 1
      if ch == "]" then return arr end
      if ch ~= "," then error("json.decode: expected , or ] got " .. ch) end
    end
  end

  parse_value = function()
    skip_ws()
    local ch = s:sub(pos, pos)
    if ch == "{" then return parse_object()
    elseif ch == "[" then return parse_array()
    elseif ch == '"' then return parse_string()
    elseif ch == "t" then pos = pos + 4; return true
    elseif ch == "f" then pos = pos + 5; return false
    elseif ch == "n" then pos = pos + 4; return nil
    else return parse_number() end
  end

  return parse_value()
end

M.json = json

--------------------------------------------------------------------------------
-- Frame encode/decode.
--------------------------------------------------------------------------------

-- encode_frame serializes a frame table to the on-wire bytes.
function M.encode_frame(frame)
  frame.v = M.PROTOCOL_VERSION
  local payload = json.encode(frame)
  return u32le(#payload) .. u32le(M.crc32(payload)) .. payload
end

-- decode_frame parses on-wire bytes into a frame table, validating length,
-- crc, and protocol version. Returns (frame, nil) or (nil, errmsg).
function M.decode_frame(bytes)
  if #bytes < 8 then return nil, "short header" end
  local n = read_u32le(bytes, 1)
  local want = read_u32le(bytes, 5)
  local payload = bytes:sub(9, 8 + n)
  if #payload ~= n then return nil, "short payload" end
  if M.crc32(payload) ~= want then return nil, "crc mismatch" end
  local frame = json.decode(payload)
  if frame.v ~= M.PROTOCOL_VERSION then
    return nil, "protocol version mismatch: got " .. tostring(frame.v)
  end
  return frame, nil
end

--------------------------------------------------------------------------------
-- Request constructors (mirror internal/wire helpers).
--------------------------------------------------------------------------------

-- gate_frame builds a GATE request frame. req fields: account, partition, uid,
-- time_limit, ntasks (and optional tres table).
function M.gate_frame(req)
  return { k = "gate", gate = req }
end

-- settle_frame builds a SETTLE request frame.
function M.settle_frame(req)
  return { k = "settle", settle = req }
end

-- bind_frame builds a BIND request frame.
function M.bind_frame(req)
  return { k = "bind", bind = req }
end

return M
