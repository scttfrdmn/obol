/*
 * obol_site_factor.c — REFERENCE site_factor plugin for the obol burst dispatch
 * gate (docs/SEAM_DESIGN.md §4, issue #14).
 *
 * ============================ REFERENCE ONLY ============================
 * This is documented, compilable reference source. It is NOT built, NOT tested
 * in CI, and NOT wired into the Slurm-22.05 Docker integration tier (that tier
 * installs Slurm from RPM and has no plugin headers). The TESTED equivalent of
 * the dispatch decision is the Go daemon handler `handleDispatch` and the
 * `obol dispatch` CLI verb — see internal/daemon/dispatch_test.go and
 * internal/cli. Treat this file as the contract-faithful skeleton a site
 * compiles against its own Slurm source tree, not as shipped, verified code.
 * ========================================================================
 *
 * WHAT IT DOES
 *   Slurm's priority/multifactor plugin invokes a site_factor hook for each
 *   pending job every scheduling cycle. There is no Lua binding for this hook
 *   (unlike JobSubmitPlugins=lua), so it must be C. For each pending job this
 *   plugin:
 *     1. reads the correlation token obol stamped into admin_comment
 *        ("budget:<hex>", set by seam/lua/job_submit.lua);
 *     2. asks obold over its Unix socket: "may this job dispatch now?" (a
 *        DISPATCH wire request — internal/wire.DispatchRequest);
 *     3. on dispatch=false, returns a site factor of 0 to HOLD the job at
 *        priority 0; on dispatch=true (or on any error / missing token) it
 *        leaves priority unchanged (FAIL-OPEN).
 *
 * The token<->jobid bind and the burst reservation at pending->running are
 * already handled by the prolog -> BIND -> bd.Start path (seam/slurm/
 * obol-prolog.sh), so this plugin's sole job is the dispatch HOLD decision.
 *
 * WHY FAIL-OPEN: if obold is unreachable, holding every job at priority 0 would
 * freeze all scheduling. The submit-time GATE is the hard budget boundary; the
 * dispatch gate only shapes concurrency, so degrading to "don't hold" is safe.
 * This mirrors the prolog's daemon-down posture.
 *
 * WIRE FRAMING (mirrors internal/wire/wire.go):
 *   [u32 len LE][u32 crc32(IEEE) LE][JSON payload]
 *   Request JSON:  {"v":1,"k":"dispatch","dispatch":{"account":"...",
 *                   "partition":"...","time_limit":<secs>,"tres":{...}}}
 *   Response JSON: {"v":1,"k":"dispatch","dispatch_resp":{"ok":true,
 *                   "dispatch":true|false,"hold":"...", ...}}
 *   ProtocolVersion is 1. The daemon rejects a frame whose "v" it doesn't know.
 *
 * BUILD (against a Slurm source tree; adjust paths):
 *   gcc -shared -fPIC -I<slurm-src>/src/common -I<slurm-src> \
 *       -o obol_site_factor.so obol_site_factor.c -lz
 *   (-lz for crc32 from zlib; or vendor a CRC-32/IEEE table.)
 *
 * slurm.conf:
 *   PriorityType=priority/multifactor
 *   PrioritySiteFactorPlugin=site_factor/obol   # install the .so accordingly
 *
 * The Slurm site_factor plugin ABI (init/fini, site_factor_p_set, the job_record
 * fields) varies across major versions; the callback body below is written to
 * the documented contract and marked where a site must adapt to its Slurm.
 */

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <zlib.h> /* crc32() — IEEE, matches Go's crc32.IEEE table */

/* --- configuration (a real plugin reads these from plugin config/env) --- */
#define OBOL_SOCKET_DEFAULT "/run/obol/obold.sock"
#define OBOL_PROTOCOL_VERSION 1
#define OBOL_MAX_FRAME (1u << 20) /* 1 MiB, mirrors wire.MaxFrameSize */

/* write_all / read_all: loop over partial IO. Return 0 on success, -1 on error. */
static int write_all(int fd, const void *buf, size_t n) {
	const char *p = buf;
	while (n > 0) {
		ssize_t w = write(fd, p, n);
		if (w <= 0) return -1;
		p += w;
		n -= (size_t)w;
	}
	return 0;
}

static int read_all(int fd, void *buf, size_t n) {
	char *p = buf;
	while (n > 0) {
		ssize_t r = read(fd, p, n);
		if (r <= 0) return -1;
		p += r;
		n -= (size_t)r;
	}
	return 0;
}

/* obol_write_frame frames payload as [u32 len LE][u32 crc32 LE][payload] and
 * writes it. Mirrors wire.WriteFrame. */
static int obol_write_frame(int fd, const char *payload, uint32_t len) {
	unsigned char hdr[8];
	uint32_t crc = (uint32_t)crc32(0L, (const unsigned char *)payload, len);
	hdr[0] = len & 0xff; hdr[1] = (len >> 8) & 0xff;
	hdr[2] = (len >> 16) & 0xff; hdr[3] = (len >> 24) & 0xff;
	hdr[4] = crc & 0xff; hdr[5] = (crc >> 8) & 0xff;
	hdr[6] = (crc >> 16) & 0xff; hdr[7] = (crc >> 24) & 0xff;
	if (write_all(fd, hdr, 8) != 0) return -1;
	return write_all(fd, payload, len);
}

/* obol_read_frame reads one framed response into buf (cap bytes). Returns the
 * payload length, or -1 on error / crc mismatch / oversize. Mirrors
 * wire.ReadFrame. */
static long obol_read_frame(int fd, char *buf, size_t cap) {
	unsigned char hdr[8];
	if (read_all(fd, hdr, 8) != 0) return -1;
	uint32_t len = (uint32_t)hdr[0] | ((uint32_t)hdr[1] << 8) |
	               ((uint32_t)hdr[2] << 16) | ((uint32_t)hdr[3] << 24);
	uint32_t want = (uint32_t)hdr[4] | ((uint32_t)hdr[5] << 8) |
	                ((uint32_t)hdr[6] << 16) | ((uint32_t)hdr[7] << 24);
	if (len > OBOL_MAX_FRAME || len >= cap) return -1;
	if (read_all(fd, buf, len) != 0) return -1;
	uint32_t got = (uint32_t)crc32(0L, (const unsigned char *)buf, len);
	if (got != want) return -1;
	buf[len] = '\0';
	return (long)len;
}

/* obol_dial connects to the obold Unix socket. Returns fd or -1. */
static int obol_dial(const char *path) {
	int fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (fd < 0) return -1;
	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
		close(fd);
		return -1;
	}
	return fd;
}

/* extract_token finds "budget:<hex>" in admin_comment (space-separated tags).
 * Copies it into out (cap bytes). Returns 1 if found, 0 otherwise. */
static int extract_token(const char *admin_comment, char *out, size_t cap) {
	if (!admin_comment) return 0;
	const char *p = strstr(admin_comment, "budget:");
	if (!p) return 0;
	size_t i = 0;
	while (p[i] && p[i] != ' ' && i < cap - 1) { out[i] = p[i]; i++; }
	out[i] = '\0';
	return i > 0;
}

/*
 * obol_may_dispatch queries obold and returns:
 *    1  -> dispatch (leave priority unchanged)
 *    0  -> HOLD (caller sets site factor 0)
 *   -1  -> error / fail-open (caller leaves priority unchanged)
 *
 * account/partition come from the job record; time_limit is the requested
 * walltime in SECONDS (Slurm stores minutes — convert at the call site).
 */
static int obol_may_dispatch(const char *sock, const char *account,
                             const char *partition, long time_limit_secs) {
	int fd = obol_dial(sock);
	if (fd < 0) return -1; /* daemon down -> fail-open */

	/* Build the request JSON. A real plugin should JSON-escape account/partition;
	 * Slurm account/partition names are constrained, so this reference keeps it
	 * simple. tres omitted here (add "tres":{"cpus":N,...} for TRES pricing). */
	char payload[512];
	int n = snprintf(payload, sizeof(payload),
	    "{\"v\":%d,\"k\":\"dispatch\",\"dispatch\":{\"account\":\"%s\","
	    "\"partition\":\"%s\",\"time_limit\":%ld}}",
	    OBOL_PROTOCOL_VERSION, account ? account : "",
	    partition ? partition : "", time_limit_secs);
	if (n <= 0 || (size_t)n >= sizeof(payload)) { close(fd); return -1; }

	if (obol_write_frame(fd, payload, (uint32_t)n) != 0) { close(fd); return -1; }

	char resp[4096];
	long rlen = obol_read_frame(fd, resp, sizeof(resp));
	close(fd);
	if (rlen < 0) return -1; /* transport/crc error -> fail-open */

	/* Minimal parse: this reference scans for the boolean fields rather than
	 * pulling in a JSON library. A production plugin should use a real parser.
	 * The response carries "dispatch":true|false inside dispatch_resp; "ok":false
	 * means the daemon rejected the query (unknown account, etc.) -> fail-open. */
	if (strstr(resp, "\"ok\":false")) return -1;
	if (strstr(resp, "\"dispatch\":true")) return 1;
	if (strstr(resp, "\"dispatch\":false")) return 0;
	return -1; /* unparseable -> fail-open */
}

/*
 * ---- Slurm site_factor callback (ADAPT TO YOUR SLURM VERSION) ----
 *
 * The exact signature and job_record field names differ across Slurm majors.
 * The shape below reflects the documented contract: for a pending job, decide a
 * site factor. Returning 0 holds the job at priority 0; leaving the existing
 * priority alone lets it schedule normally.
 *
 * Pseudocode against a typical job_record (fields named illustratively):
 *
 *   extern void site_factor_p_set(job_record_t *job_ptr) {
 *       const char *sock = getenv("OBOL_SOCKET");
 *       if (!sock) sock = OBOL_SOCKET_DEFAULT;
 *
 *       char token[128];
 *       if (!extract_token(job_ptr->admin_comment, token, sizeof(token)))
 *           return;                       // ungated job: leave priority alone
 *
 *       long secs = (job_ptr->time_limit == NO_VAL)
 *                       ? 0 : (long)job_ptr->time_limit * 60;  // minutes -> secs
 *
 *       int v = obol_may_dispatch(sock, job_ptr->account,
 *                                 job_ptr->partition, secs);
 *       if (v == 0)                        // no burst headroom: HOLD
 *           job_ptr->site_factor = 0;      // (or the plugin's hold mechanism)
 *       // v == 1 (dispatch) or v == -1 (fail-open): leave priority unchanged
 *   }
 *
 * Note: obol_may_dispatch reuses `token` only to confirm the job is gated; the
 * daemon resolves the budget from `account` (+partition for node-type pricing),
 * exactly like handleDispatch. The token is the "is this an obol job?" signal.
 */
