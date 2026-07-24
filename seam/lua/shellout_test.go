// Package luawire_test also covers the GATE shim's CLI shell-out fallback (#137):
// when no in-process Lua socket backend is available (no luasocket, no LuaJIT FFI
// — the case on minimal managed AMIs like AWS ParallelCluster / PCS), the shim
// exec's the `obol` CLI to gate instead of failing closed. This test forces the
// in-process transport to fail, runs the real job_submit.lua against a real
// obold, and asserts the fallback produces a correct allow/reject decision.
package luawire_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildBin compiles a command from the module root into dir and returns its path.
func buildBin(t *testing.T, dir, cmd string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // seam/lua -> module root
	out := filepath.Join(dir, cmd)
	b := exec.Command("go", "build", "-o", out, "./cmd/"+cmd)
	b.Dir = root
	if o, err := b.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", cmd, err, o)
	}
	return out
}

// runShimGate runs job_submit.lua's gate path once against obold at socket, with
// the in-process transport forced to fail so the CLI shell-out (#137) is
// exercised. obolBin is the CLI the shim will exec. It returns the Slurm rc
// (0=SUCCESS, -1=ERROR) and the job's admin_comment after the call.
func runShimGate(t *testing.T, lua, obolBin, socket, account, partition string, timeLimit int) (string, string) {
	t.Helper()
	wd, _ := os.Getwd()
	script := `
package.path = '` + filepath.Join(wd, "?.lua") + `;' .. package.path
-- Force the in-process transport to fail so the shell-out fallback runs.
local transport = require("obol_transport")
transport.round_trip = function() return nil, "forced: no in-process backend (test)" end
dofile('` + filepath.Join(wd, "job_submit.lua") + `')
local jd = { account = '` + account + `', partition = '` + partition + `', time_limit = ` + strconv.Itoa(timeLimit) + ` }
local rc = slurm_job_submit(jd, nil, 0)
print('rc=' .. tostring(rc))
print('ac=' .. tostring(jd.admin_comment))
`
	cmd := exec.Command(lua, "-e", script)
	cmd.Env = append(os.Environ(), "OBOL_BIN="+obolBin, "OBOL_SOCKET="+socket)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("lua shim run failed: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	var rc, ac string
	for _, line := range strings.Split(out.String(), "\n") {
		if s, ok := strings.CutPrefix(line, "rc="); ok {
			rc = strings.TrimSpace(s)
		}
		if s, ok := strings.CutPrefix(line, "ac="); ok {
			ac = strings.TrimSpace(s)
		}
	}
	return rc, ac
}

// TestShimGateCLIShellout drives the GATE shim's #137 fallback end to end: with
// the in-process transport forced to fail, a funded job must gate via the `obol`
// CLI (SUCCESS + a stamped budget token) and an unfundable job must be rejected
// (ERROR, no token). Proves obol still enforces on a host with no Lua socket
// backend.
func TestShimGateCLIShellout(t *testing.T) {
	lua := luaBin(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	obold := buildBin(t, dir, "obold")
	obol := buildBin(t, dir, "obol")
	socket := filepath.Join(dir, "obold.sock")

	// A small balance: funds a 60s job (60 units) but not a huge one.
	d := exec.Command(obold, "-socket", socket, "-state-dir", filepath.Join(dir, "state"),
		"-create", "-balance", "100000", "-rate", "1", "-sync=false")
	if err := d.Start(); err != nil {
		t.Fatalf("start obold: %v", err)
	}
	t.Cleanup(func() { _ = d.Process.Kill(); _, _ = d.Process.Wait() })
	waitForSocket(t, socket)

	// Funded: 1-minute job → allowed via the CLI fallback, token stamped.
	rc, ac := runShimGate(t, lua, obol, socket, "default", "cloud", 1)
	if rc != "0" {
		t.Errorf("funded job via CLI fallback: rc=%s, want 0 (SUCCESS)", rc)
	}
	if !strings.Contains(ac, "budget:") {
		t.Errorf("funded job: admin_comment=%q, want a budget: token", ac)
	}

	// Unfundable: a very long job exceeds the balance → rejected, no token.
	rc, ac = runShimGate(t, lua, obol, socket, "default", "cloud", 100000)
	if rc != "-1" {
		t.Errorf("unfunded job via CLI fallback: rc=%s, want -1 (ERROR)", rc)
	}
	if strings.Contains(ac, "budget:") {
		t.Errorf("unfunded job: admin_comment=%q, want no token", ac)
	}
}

// waitForSocket blocks until the daemon's socket exists (or fails the test).
func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("obold socket %s never appeared", socket)
}
