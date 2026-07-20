// Package luawire_test cross-validates the pure-Lua wire module
// (seam/lua/obol_wire.lua) against the Go reference (internal/wire): a frame
// encoded by one side must decode on the other, byte for byte. This is what
// lets the Lua job_submit shim speak the daemon's protocol without a Go client.
//
// The test is skipped when no `lua` interpreter is on PATH, so it never blocks a
// developer without Lua installed; CI and the Docker tier have it.
package luawire_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// luaBin finds a Lua 5.3+ interpreter, or skips the test.
func luaBin(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"lua", "lua5.4", "lua5.3", "luajit"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no lua interpreter on PATH; skipping Lua↔Go wire cross-validation")
	return ""
}

// runLua runs a Lua script with the seam/lua dir on package.path and returns its
// raw stdout bytes.
func runLua(t *testing.T, lua, script string) []byte {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	preamble := "package.path = '" + filepath.Join(wd, "?.lua") + ";' .. package.path\n"
	cmd := exec.Command(lua, "-e", preamble+script)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("lua failed: %v\nstderr: %s", err, errOut.String())
	}
	return out.Bytes()
}

// TestLuaEncodeGoDecode: a frame the Lua module encodes decodes correctly in Go.
func TestLuaEncodeGoDecode(t *testing.T) {
	lua := luaBin(t)
	cases := []struct {
		name   string
		script string
		check  func(t *testing.T, f *wire.Frame)
	}{
		{
			"gate",
			`local w=require("obol_wire")
			 io.write(w.encode_frame(w.gate_frame({account="lab",partition="cloud",uid=1001,time_limit=1000,ntasks=1})))`,
			func(t *testing.T, f *wire.Frame) {
				if f.MsgKind != wire.KindGate || f.Gate == nil {
					t.Fatalf("kind=%s gate=%v", f.MsgKind, f.Gate)
				}
				g := f.Gate
				if g.Account != "lab" || g.Partition != "cloud" || g.UID != 1001 || g.TimeLimit != 1000 || g.NTasks != 1 {
					t.Errorf("gate mismatch: %+v", g)
				}
			},
		},
		{
			"settle",
			`local w=require("obol_wire")
			 io.write(w.encode_frame(w.settle_frame({jobid="42",kind="complete",runtime=300})))`,
			func(t *testing.T, f *wire.Frame) {
				if f.Settle == nil || f.Settle.JobID != "42" || f.Settle.Kind != wire.SettleComplete || f.Settle.Runtime != 300 {
					t.Errorf("settle mismatch: %+v", f.Settle)
				}
			},
		},
		{
			"bind",
			`local w=require("obol_wire")
			 io.write(w.encode_frame(w.bind_frame({token="budget:abc",jobid="7"})))`,
			func(t *testing.T, f *wire.Frame) {
				if f.Bind == nil || f.Bind.Token != "budget:abc" || f.Bind.JobID != "7" {
					t.Errorf("bind mismatch: %+v", f.Bind)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := runLua(t, lua, tc.script)
			f, err := wire.ReadFrame(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("Go ReadFrame of Lua-encoded frame: %v", err)
			}
			tc.check(t, f)
		})
	}
}

// TestGoEncodeLuaDecode: a frame Go encodes decodes correctly in the Lua module.
// We write a GateResponse (allow+token) — the shim must read exactly this.
func TestGoEncodeLuaDecode(t *testing.T) {
	lua := luaBin(t)

	var buf bytes.Buffer
	if err := wire.WriteFrame(&buf, &wire.Frame{
		MsgKind:  wire.KindGate,
		GateResp: &wire.GateResponse{Allow: true, Token: "budget:deadbeef"},
	}); err != nil {
		t.Fatal(err)
	}

	// Pass the Go-encoded bytes to Lua via a temp file; Lua decodes and prints
	// fields we assert on.
	tmp := filepath.Join(t.TempDir(), "frame.bin")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `local w=require("obol_wire")
	           local fh=io.open("` + tmp + `","rb"); local bytes=fh:read("*a"); fh:close()
	           local f,err=w.decode_frame(bytes)
	           if not f then error(err) end
	           io.write(f.k, "|", tostring(f.gate_resp.allow), "|", f.gate_resp.token)`
	out := string(runLua(t, lua, script))
	want := "gate|true|budget:deadbeef"
	if strings.TrimSpace(out) != want {
		t.Errorf("Lua decode of Go frame = %q, want %q", out, want)
	}
}

// TestCRC32MatchesGo confirms the Lua crc32 equals Go's crc32.IEEE on the
// canonical check vector, so frames validate across the boundary.
func TestCRC32MatchesGo(t *testing.T) {
	lua := luaBin(t)
	out := string(runLua(t, lua,
		`local w=require("obol_wire"); io.write(tostring(w.crc32("123456789")))`))
	if strings.TrimSpace(out) != "3421780262" { // canonical IEEE CRC-32 check value
		t.Errorf("Lua crc32(123456789) = %q, want 3421780262", out)
	}
}
