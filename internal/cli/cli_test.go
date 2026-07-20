package cli

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/daemon"
)

// newDaemon starts an in-process obold over a Unix socket in a temp dir and
// returns the socket path and the underlying budget for assertions. now is fixed
// at 1 so the window [0,100000) is open.
func newDaemon(t *testing.T) (string, *budget.Budget) {
	t.Helper()
	dir := t.TempDir()
	bd, err := budget.NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatalf("NewDurable: %v", err)
	}
	srv := daemon.New(bd, func() budget.Seconds { return 1 })
	sock := filepath.Join(dir, "obold.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = bd.Close()
	})
	return sock, bd
}

// run invokes a verb and returns exit code, stdout, stderr.
func run(sock string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	full := make([]string, 0, len(args)+2)
	full = append(full, args...)
	full = append(full, "--socket", sock)
	code := Run(full, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPingVerb(t *testing.T) {
	sock, _ := newDaemon(t)
	code, out, errOut := run(sock, "ping")
	if code != 0 {
		t.Fatalf("ping exit %d, stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("ping out = %q, want ok", out)
	}
}

// TestGlobalFlagBeforeVerb confirms --socket works when it precedes the verb
// (obol --socket X ping), not just after it. Both forms must be equivalent.
func TestGlobalFlagBeforeVerb(t *testing.T) {
	sock, _ := newDaemon(t)
	var out, errOut bytes.Buffer
	// Leading form, and --socket=VAL syntax.
	code := Run([]string{"--socket=" + sock, "ping"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("leading --socket= exit %d, stderr=%q", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "ok" {
		t.Errorf("out = %q, want ok", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{"--socket", sock, "show"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "Balance:") {
		t.Errorf("leading --socket show: exit %d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestShowVerb(t *testing.T) {
	sock, _ := newDaemon(t)
	code, out, errOut := run(sock, "show")
	if code != 0 {
		t.Fatalf("show exit %d, stderr=%q", code, errOut)
	}
	for _, want := range []string{"Balance:", "100000", "Conservation:", "OK", "Time-to-empty:"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q\n%s", want, out)
		}
	}
}

func TestGateBindSettleVerbs(t *testing.T) {
	sock, bd := newDaemon(t)

	// GATE a 1000s job -> allow + token.
	code, out, errOut := run(sock, "gate", "--account", "lab", "--partition", "cloud", "--time-limit", "1000")
	if code != 0 {
		t.Fatalf("gate exit %d, stderr=%q", code, errOut)
	}
	if !strings.HasPrefix(out, "allow budget:") {
		t.Fatalf("gate out = %q, want 'allow budget:...'", out)
	}
	token := strings.TrimSpace(strings.TrimPrefix(out, "allow "))
	if bal := bd.Balance(); bal != 99000 {
		t.Errorf("after gate balance = %d, want 99000", bal)
	}

	// BIND token -> jobid.
	if code, _, errOut := run(sock, "bind", "--token", token, "--jobid", "7"); code != 0 {
		t.Fatalf("bind exit %d, stderr=%q", code, errOut)
	}

	// SETTLE complete by jobid, runtime 250 -> refund 750.
	if code, _, errOut := run(sock, "settle", "--jobid", "7", "--kind", "complete", "--runtime", "250"); code != 0 {
		t.Fatalf("settle exit %d, stderr=%q", code, errOut)
	}
	if bal := bd.Balance(); bal != 99750 {
		t.Errorf("after settle balance = %d, want 99750", bal)
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation violated after settle")
	}
}

// TestGateRejectExitCode confirms a clean rejection returns exit 3 (distinct
// from a transport error's exit 1), so scripts can tell "no" from "unreachable".
func TestGateRejectExitCode(t *testing.T) {
	sock, _ := newDaemon(t)
	code, out, _ := run(sock, "gate", "--account", "lab", "--partition", "cloud", "--time-limit", "200000")
	if code != 3 {
		t.Fatalf("over-budget gate exit = %d, want 3", code)
	}
	if !strings.HasPrefix(out, "reject:") {
		t.Errorf("gate out = %q, want 'reject:...'", out)
	}
}

// TestTransportErrorExitCode confirms an unreachable daemon returns exit 1.
func TestTransportErrorExitCode(t *testing.T) {
	code, _, errOut := run(filepath.Join(t.TempDir(), "nonexistent.sock"), "show")
	if code != 1 {
		t.Fatalf("unreachable daemon exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "connect to obold") {
		t.Errorf("stderr = %q, want a connect error", errOut)
	}
}

// TestArrayGateVerb drives an array submission through the gate verb.
func TestArrayGateVerb(t *testing.T) {
	sock, bd := newDaemon(t)
	code, out, errOut := run(sock, "gate", "--account", "lab", "--partition", "cloud", "--time-limit", "100", "--ntasks", "10")
	if code != 0 {
		t.Fatalf("array gate exit %d, stderr=%q", code, errOut)
	}
	if !strings.HasPrefix(out, "allow ") {
		t.Errorf("array gate out = %q", out)
	}
	if bal := bd.Balance(); bal != 99000 { // 10*100*1
		t.Errorf("after array gate balance = %d, want 99000", bal)
	}
}

func TestBadArgs(t *testing.T) {
	sock, _ := newDaemon(t)
	// settle with neither jobid nor token.
	if code, _, _ := run(sock, "settle", "--kind", "complete"); code != 1 {
		t.Errorf("settle w/o id: exit = %d, want 1", code)
	}
	// gate with no time-limit.
	if code, _, _ := run(sock, "gate", "--account", "a", "--partition", "p"); code != 1 {
		t.Errorf("gate w/o time-limit: exit = %d, want 1", code)
	}
	// unknown verb.
	if code := Run([]string{"frobnicate"}, new(bytes.Buffer), new(bytes.Buffer)); code != 2 {
		t.Errorf("unknown verb: exit = %d, want 2", code)
	}
}
