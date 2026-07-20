-- obol_transport.lua — one-shot Unix-socket round-trip for the GATE call.
--
-- The shim needs to send one framed request and read one framed response over a
-- Unix stream socket, with a hard timeout (a hung daemon must never stall the
-- controller — docs/SEAM_DESIGN.md §7). Two backends, tried in order:
--
--   1. luasocket (socket.unix) — the common case; installed in the Docker/PC
--      images and typical on cluster controllers.
--   2. LuaJIT FFI (connect/send/recv on an AF_UNIX socket) — fallback when the
--      controller runs LuaJIT without luasocket.
--
-- round_trip(path, request_bytes, timeout_ms) returns (response_bytes) on
-- success, or (nil, errmsg) on any failure so the caller applies fail-closed.

local M = {}

-- read_framed reads a full obol frame from a stream given a byte-reader
-- function recv(n) -> string|nil. Reads the 8-byte header, then the payload.
local function read_framed(recv)
  local hdr, herr = recv(8)
  if not hdr or #hdr < 8 then return nil, "short header (" .. tostring(herr) .. ")" end
  local a, b, c, d = string.byte(hdr, 1, 4)
  local n = a + b * 256 + c * 65536 + d * 16777216
  local payload = recv(n)
  if not payload or #payload < n then return nil, "short payload" end
  return hdr .. payload
end

--------------------------------------------------------------------------------
-- Backend 1: luasocket
--------------------------------------------------------------------------------

local function try_luasocket(path, request, timeout_s)
  local ok, unix = pcall(require, "socket.unix")
  if not ok or not unix then return nil, "no luasocket" end

  -- luasocket 3.x: socket.unix.stream() makes a SOCK_STREAM socket. The bare
  -- socket.unix() call may yield a datagram socket, which silently drops the
  -- framed response (seen as a "short header"). Always ask for a stream.
  local sock
  if type(unix) == "table" and type(unix.stream) == "function" then
    sock = unix.stream()
  else
    sock = unix() -- older luasocket: callable module returns a stream socket
  end
  sock:settimeout(timeout_s)
  local cok, cerr = sock:connect(path)
  if not cok then
    sock:close()
    return nil, "connect: " .. tostring(cerr)
  end
  -- Ensure the whole request is written (send may do a partial write; loop on
  -- the returned index until all bytes are out).
  local sent = 0
  while sent < #request do
    local i, serr = sock:send(request, sent + 1)
    if not i then
      sock:close()
      return nil, "send: " .. tostring(serr)
    end
    sent = i
  end

  -- Accumulate to an exact byte count. Inside slurmctld's embedded interpreter
  -- receive(n) can return fewer than n bytes with a "timeout" partial; the
  -- partial is the third return value, so keep pulling until we have n.
  local buf = ""
  local resp, rerr = read_framed(function(n)
    while #buf < n do
      local chunk, e, partial = sock:receive(n - #buf)
      if chunk then
        buf = buf .. chunk
      elseif partial and #partial > 0 then
        buf = buf .. partial
      else
        return nil, e
      end
    end
    local out = buf:sub(1, n)
    buf = buf:sub(n + 1)
    return out
  end)
  sock:close()
  if not resp then return nil, rerr end
  return resp
end

--------------------------------------------------------------------------------
-- Backend 2: LuaJIT FFI (AF_UNIX SOCK_STREAM)
--------------------------------------------------------------------------------

local function try_ffi(path, request, timeout_s)
  local ok, ffi = pcall(require, "ffi")
  if not ok then return nil, "no ffi" end

  ffi.cdef [[
    int socket(int domain, int type, int protocol);
    int connect(int sockfd, const void *addr, unsigned int addrlen);
    long write(int fd, const void *buf, unsigned long count);
    long read(int fd, void *buf, unsigned long count);
    int close(int fd);
    int setsockopt(int fd, int level, int optname, const void *optval, unsigned int optlen);
    struct sockaddr_un { unsigned short sun_family; char sun_path[108]; };
    struct timeval { long tv_sec; long tv_usec; };
  ]]

  local AF_UNIX, SOCK_STREAM = 1, 1
  local SOL_SOCKET, SO_RCVTIMEO, SO_SNDTIMEO = 1, 20, 21 -- Linux values

  local fd = ffi.C.socket(AF_UNIX, SOCK_STREAM, 0)
  if fd < 0 then return nil, "socket()" end

  local tv = ffi.new("struct timeval", { math.floor(timeout_s), (timeout_s % 1) * 1e6 })
  ffi.C.setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, tv, ffi.sizeof(tv))
  ffi.C.setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, tv, ffi.sizeof(tv))

  local addr = ffi.new("struct sockaddr_un")
  addr.sun_family = AF_UNIX
  ffi.copy(addr.sun_path, path)
  if ffi.C.connect(fd, addr, ffi.sizeof(addr)) ~= 0 then
    ffi.C.close(fd)
    return nil, "connect()"
  end

  if ffi.C.write(fd, request, #request) < 0 then
    ffi.C.close(fd)
    return nil, "write()"
  end

  local buf = ffi.new("char[?]", 65536)
  local acc = {}
  local resp, rerr = read_framed(function(n)
    while true do
      -- Read what's buffered; accumulate until we have n bytes for this call.
      local have = 0
      for _, s in ipairs(acc) do have = have + #s end
      if have >= n then break end
      local got = ffi.C.read(fd, buf, 65536)
      if got <= 0 then return nil, "read()" end
      acc[#acc + 1] = ffi.string(buf, got)
    end
    local all = table.concat(acc)
    local out = all:sub(1, n)
    acc = { all:sub(n + 1) }
    return out
  end)
  ffi.C.close(fd)
  if not resp then return nil, rerr end
  return resp
end

-- round_trip sends request over the Unix socket at path and returns the framed
-- response bytes, honoring timeout_ms as a hard cap. Returns (nil, err) on any
-- failure; the caller then applies its fail-closed policy.
function M.round_trip(path, request, timeout_ms)
  local timeout_s = (timeout_ms or 50) / 1000
  local resp, err = try_luasocket(path, request, timeout_s)
  if resp then return resp end
  local ls_err = err
  resp, err = try_ffi(path, request, timeout_s)
  if resp then return resp end
  return nil, "no usable socket backend (luasocket: " .. tostring(ls_err) .. "; ffi: " .. tostring(err) .. ")"
end

return M
