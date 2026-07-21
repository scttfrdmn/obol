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

	// A second settle of the same job (double-fire from jobcomp + epilog) is a
	// hard error by default but a benign no-op with --if-present.
	if code, _, _ := run(sock, "settle", "--jobid", "7", "--kind", "complete", "--runtime", "10"); code != 1 {
		t.Errorf("double settle without --if-present: exit = %d, want 1", code)
	}
	if code, out, errOut := run(sock, "settle", "--jobid", "7", "--kind", "complete", "--runtime", "10", "--if-present"); code != 0 {
		t.Errorf("double settle with --if-present: exit = %d, stderr=%q", code, errOut)
	} else if !strings.Contains(out, "already settled") {
		t.Errorf("--if-present out = %q, want 'already settled'", out)
	}
	// Balance must be unchanged by the no-op re-settle.
	if bal := bd.Balance(); bal != 99750 {
		t.Errorf("balance changed by no-op re-settle: %d", bal)
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

// TestTopUpAndListVerbs drives the topup and list verbs against a single-budget
// daemon (admin enforcement off, so topup is allowed). Confirms topup raises the
// balance and list shows the account.
func TestTopUpAndListVerbs(t *testing.T) {
	sock, bd := newDaemon(t)

	// topup default by 5000.
	code, out, errOut := run(sock, "topup", "--account", "default", "--amount", "5000")
	if code != 0 {
		t.Fatalf("topup exit %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "105000") { // 100000 + 5000
		t.Errorf("topup out = %q, want new balance 105000", out)
	}
	if bd.Balance() != 105000 {
		t.Errorf("balance after topup = %d, want 105000", bd.Balance())
	}

	// list shows the account and its balance.
	code, out, errOut = run(sock, "list")
	if code != 0 {
		t.Fatalf("list exit %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "default") || !strings.Contains(out, "105000") {
		t.Errorf("list out missing account/balance:\n%s", out)
	}
}

// TestTopUpBadArgs confirms topup requires an account and a positive amount.
func TestTopUpBadArgs(t *testing.T) {
	sock, _ := newDaemon(t)
	if code, _, _ := run(sock, "topup", "--amount", "100"); code != 1 {
		t.Errorf("topup w/o account: exit = %d, want 1", code)
	}
	if code, _, _ := run(sock, "topup", "--account", "default", "--amount", "0"); code != 1 {
		t.Errorf("topup with zero amount: exit = %d, want 1", code)
	}
}

// TestLogVerb drives the log verb: submit+settle a job, then confirm the log
// renders both transitions.
func TestLogVerb(t *testing.T) {
	sock, _ := newDaemon(t)

	// Gate + settle a job so there are transitions to show.
	code, out, _ := run(sock, "gate", "--account", "default", "--partition", "cloud", "--time-limit", "1000")
	if code != 0 {
		t.Fatalf("gate exit %d", code)
	}
	tok := strings.TrimSpace(strings.TrimPrefix(out, "allow "))
	run(sock, "bind", "--token", tok, "--jobid", "9")
	run(sock, "settle", "--jobid", "9", "--kind", "complete", "--runtime", "100")

	code, out, errOut := run(sock, "log", "--account", "default")
	if code != 0 {
		t.Fatalf("log exit %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "submit") || !strings.Contains(out, "settle:complete") {
		t.Errorf("log missing expected transitions:\n%s", out)
	}
}

// TestSetRateWindowVerbs drives set-rate and set-window against a single-budget
// daemon (admin enforcement off) and confirms the config changes take.
func TestSetRateWindowVerbs(t *testing.T) {
	sock, bd := newDaemon(t)

	if code, out, errOut := run(sock, "set-rate", "--account", "default", "--rate", "7"); code != 0 {
		t.Fatalf("set-rate exit %d, stderr=%q", code, errOut)
	} else if !strings.Contains(out, "set to 7") {
		t.Errorf("set-rate out = %q", out)
	}
	if bd.Report(1).C != 7 {
		t.Errorf("rate = %d, want 7", bd.Report(1).C)
	}

	// set-window with --window duration.
	if code, _, errOut := run(sock, "set-window", "--account", "default", "--window", "1h"); code != 0 {
		t.Fatalf("set-window --window exit %d, stderr=%q", code, errOut)
	}
	// set-window with explicit epoch start/end.
	if code, _, errOut := run(sock, "set-window", "--account", "default", "--start", "0", "--end", "500000"); code != 0 {
		t.Fatalf("set-window --start/--end exit %d, stderr=%q", code, errOut)
	}
	if r := bd.Report(1); r.TS != 0 || r.TE != 500000 {
		t.Errorf("window = [%d,%d), want [0,500000)", r.TS, r.TE)
	}
}

// TestSetVerbsBadArgs covers required-arg validation.
func TestSetVerbsBadArgs(t *testing.T) {
	sock, _ := newDaemon(t)
	if code, _, _ := run(sock, "set-rate", "--account", "default", "--rate", "0"); code != 1 {
		t.Errorf("set-rate rate=0: exit %d, want 1", code)
	}
	if code, _, _ := run(sock, "set-window", "--account", "default"); code != 1 {
		t.Errorf("set-window with no window/start/end: exit %d, want 1", code)
	}
	if code, _, _ := run(sock, "set-window", "--account", "default", "--start", "bogus", "--end", "5"); code != 1 {
		t.Errorf("set-window bad start: exit %d, want 1", code)
	}
}

// TestResolveVerb drives resolve against a single-budget daemon: a funded
// resolve admits (exit 0), an over-budget one is rejected (exit 3).
func TestResolveVerb(t *testing.T) {
	sock, _ := newDaemon(t) // default: balance 100000, rate 1

	code, out, errOut := run(sock, "resolve", "--account", "default", "--time-limit", "100")
	if code != 0 {
		t.Fatalf("resolve exit %d, stderr=%q", code, errOut)
	}
	for _, want := range []string{"Resolved:   true", "Admits:     true", "Rate:", "flat"} {
		if !strings.Contains(out, want) {
			t.Errorf("resolve output missing %q:\n%s", want, out)
		}
	}
	// Over budget -> would be rejected -> exit 3.
	if code, out, _ := run(sock, "resolve", "--account", "default", "--time-limit", "200000"); code != 3 {
		t.Errorf("over-budget resolve exit = %d, want 3\n%s", code, out)
	}
	// (In single-budget mode any account resolves to the sole "default" budget —
	// the back-compat rule — so unknown-account rejection is covered by the
	// multi-account daemon test, not here.)
}

// TestSimulateVerb drives simulate against the single-budget daemon (balance
// 100000, rate 1): a funded job reports WOULD FUND (exit 0), an over-budget one
// WOULD NOT FUND (exit 3).
func TestSimulateVerb(t *testing.T) {
	sock, _ := newDaemon(t)

	code, out, errOut := run(sock, "simulate", "--account", "default", "--time-limit", "100")
	if code != 0 {
		t.Fatalf("simulate exit %d, stderr=%q", code, errOut)
	}
	for _, want := range []string{"WOULD FUND", "Cost:", "Runway:"} {
		if !strings.Contains(out, want) {
			t.Errorf("simulate output missing %q:\n%s", want, out)
		}
	}
	if code, out, _ := run(sock, "simulate", "--account", "default", "--time-limit", "200000"); code != 3 {
		t.Errorf("over-budget simulate exit = %d, want 3\n%s", code, out)
	}
	// estimate is an alias.
	if code, _, _ := run(sock, "estimate", "--account", "default", "--time-limit", "100"); code != 0 {
		t.Errorf("estimate alias exit = %d, want 0", code)
	}
	// Missing time-limit is a usage error.
	if code, _, _ := run(sock, "simulate", "--account", "default"); code != 1 {
		t.Errorf("simulate w/o time-limit exit = %d, want 1", code)
	}
}
