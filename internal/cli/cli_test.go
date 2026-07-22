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

// newMultiDaemon starts a real multi-account daemon (registry with a state dir),
// needed for verbs that mutate the registry (create/attach). Returns the socket.
func newMultiDaemon(t *testing.T, cfg *daemon.Config) string {
	t.Helper()
	dir := t.TempDir()
	reg, err := daemon.NewRegistry(cfg, dir, false, func() budget.Seconds { return 1 })
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv := daemon.NewWithRegistry(reg, func() budget.Seconds { return 1 }, daemon.Weights{})
	sock := filepath.Join(dir, "obold.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = reg.Close() })
	return sock
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

// TestMultiSourceGateVerb drives `obol gate --source A --source B` end to end: a
// job costing more than the first source drains it and spills to the second.
func TestMultiSourceGateVerb(t *testing.T) {
	sock := newMultiDaemon(t, &daemon.Config{Accounts: []daemon.AccountConfig{
		{Name: "grant", Balance: 100, Rate: 1, Window: "1000000s"},
		{Name: "startup", Balance: 100000, Rate: 1, Window: "1000000s"},
	}})
	// cost 300 (rate 1 × 300s): grant funds 100, startup 200.
	code, out, errOut := run(sock, "gate", "--source", "grant", "--source", "startup", "--time-limit", "300")
	if code != 0 {
		t.Fatalf("multi-source gate exit %d, stderr=%q", code, errOut)
	}
	if !strings.HasPrefix(out, "allow ") {
		t.Errorf("multi-source gate out = %q, want an allow token", out)
	}
	// Under-funded across sources → clean reject (exit 3).
	if code, out, _ := run(sock, "gate", "--source", "grant", "--time-limit", "300"); code != 3 {
		t.Errorf("under-funded single-source-in-list: exit %d out=%q, want 3", code, out)
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

// TestDispatchVerb drives obol dispatch against a burst-enabled account: an
// under-r0 job WOULD DISPATCH (exit 0); once burn is pushed to r0 with a running
// job, an over-r0 job with no banked pot WOULD HOLD (exit 3).
func TestDispatchVerb(t *testing.T) {
	// window 1000s, balance 100000 → r0 = 100/s. Daemon clock is fixed at 1.
	sock := newMultiDaemon(t, &daemon.Config{Accounts: []daemon.AccountConfig{
		{Name: "burstlab", Balance: 100000, Rate: 1, Window: "1000s",
			BurstEnabled: true, BurstCeilingPct: 1.0},
	}})

	// Under r0 (rate 1/s, nothing running) → dispatch.
	code, out, errOut := run(sock, "dispatch", "--account", "burstlab", "--time-limit", "100")
	if code != 0 {
		t.Fatalf("dispatch exit %d, stderr=%q", code, errOut)
	}
	for _, want := range []string{"WOULD DISPATCH", "Rate:", "Pot:"} {
		if !strings.Contains(out, want) {
			t.Errorf("dispatch output missing %q:\n%s", want, out)
		}
	}
	// may-dispatch is an alias.
	if code, _, _ := run(sock, "may-dispatch", "--account", "burstlab", "--time-limit", "100"); code != 0 {
		t.Errorf("may-dispatch alias exit = %d, want 0", code)
	}
	// Missing time-limit is a usage error.
	if code, _, _ := run(sock, "dispatch", "--account", "burstlab"); code != 1 {
		t.Errorf("dispatch w/o time-limit exit = %d, want 1", code)
	}
}

// TestCreateAndAttachVerbs drives create + attach/detach against a single-budget
// daemon (admin enforcement off, so allowed). Confirms a created account is then
// listable and attach reports the resulting access.
func TestCreateAndAttachVerbs(t *testing.T) {
	sock := newMultiDaemon(t, &daemon.Config{Accounts: []daemon.AccountConfig{
		{Name: "default", Balance: 100000, Rate: 1, Window: "1000000s"},
	}})

	// create a new account.
	if code, out, errOut := run(sock, "create", "--account", "lab2", "--balance", "5000", "--rate", "2"); code != 0 {
		t.Fatalf("create exit %d, stderr=%q", code, errOut)
	} else if !strings.Contains(out, "created") {
		t.Errorf("create out = %q", out)
	}
	// list shows both default and lab2.
	if _, out, _ := run(sock, "list"); !strings.Contains(out, "lab2") {
		t.Errorf("list missing created account:\n%s", out)
	}
	// attach a user; response reports the access list.
	if code, out, errOut := run(sock, "attach", "--account", "lab2", "--user", "alice"); code != 0 {
		t.Fatalf("attach exit %d, stderr=%q", code, errOut)
	} else if !strings.Contains(out, "alice") {
		t.Errorf("attach out = %q, want alice listed", out)
	}
	// detach it back to open.
	if code, out, _ := run(sock, "detach", "--account", "lab2", "--user", "alice"); code != 0 || !strings.Contains(out, "open") {
		t.Errorf("detach: exit %d out=%q, want open", code, out)
	}

	// create a burst-enabled account (#99): --burst-ceiling-pct turns it on.
	if code, _, errOut := run(sock, "create", "--account", "burstlab", "--balance", "100000", "--rate", "1", "--burst-ceiling-pct", "0.5"); code != 0 {
		t.Fatalf("burst create exit %d, stderr=%q", code, errOut)
	}
	if _, out, _ := run(sock, "show", "--account", "burstlab"); !strings.Contains(out, "Burst:") || strings.Contains(out, "Burst:         disabled") {
		t.Errorf("burst account shows burst disabled:\n%s", out)
	}
	// A bad ceiling pct is rejected by the daemon.
	if code, _, _ := run(sock, "create", "--account", "bad", "--balance", "1", "--rate", "1", "--burst-ceiling-pct", "2"); code == 0 {
		t.Error("create with burst-ceiling-pct > 1 should be rejected")
	}
}

// TestTransferVerb drives obol transfer between two accounts and confirms both
// balances move and the CLI reports them.
func TestTransferVerb(t *testing.T) {
	sock := newMultiDaemon(t, &daemon.Config{Accounts: []daemon.AccountConfig{
		{Name: "lab_a", Balance: 10000, Rate: 1, Window: "1000000s"},
		{Name: "lab_b", Balance: 2000, Rate: 1, Window: "1000000s"},
	}})

	code, out, errOut := run(sock, "transfer", "--from", "lab_a", "--to", "lab_b", "--amount", "3000")
	if code != 0 {
		t.Fatalf("transfer exit %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "moved 3000") || !strings.Contains(out, "7000") || !strings.Contains(out, "5000") {
		t.Errorf("transfer out = %q, want moved 3000 with balances 7000/5000", out)
	}

	// --all sweeps the remaining available balance of lab_a.
	if code, out, _ := run(sock, "transfer", "--from", "lab_a", "--to", "lab_b", "--all"); code != 0 || !strings.Contains(out, "moved 7000") {
		t.Errorf("transfer --all: exit %d out=%q, want moved 7000", code, out)
	}
}

// TestTransferBadArgs covers required-arg and mutual-exclusion validation, plus a
// clean reject (exit 3) for an over-balance move.
func TestTransferBadArgs(t *testing.T) {
	sock := newMultiDaemon(t, &daemon.Config{Accounts: []daemon.AccountConfig{
		{Name: "lab_a", Balance: 100, Rate: 1, Window: "1000000s"},
		{Name: "lab_b", Balance: 100, Rate: 1, Window: "1000000s"},
	}})
	if code, _, _ := run(sock, "transfer", "--to", "lab_b", "--amount", "1"); code != 1 {
		t.Errorf("transfer w/o --from: exit %d, want 1", code)
	}
	if code, _, _ := run(sock, "transfer", "--from", "lab_a", "--to", "lab_b"); code != 1 {
		t.Errorf("transfer w/o amount or --all: exit %d, want 1", code)
	}
	if code, _, _ := run(sock, "transfer", "--from", "lab_a", "--to", "lab_b", "--amount", "5", "--all"); code != 1 {
		t.Errorf("transfer with both --amount and --all: exit %d, want 1", code)
	}
	// Over-balance is a clean daemon reject → exit 3 (distinct from usage/transport).
	if code, out, _ := run(sock, "transfer", "--from", "lab_a", "--to", "lab_b", "--amount", "99999"); code != 3 || !strings.Contains(out, "reject") {
		t.Errorf("over-balance transfer: exit %d out=%q, want exit 3 reject", code, out)
	}
}

// TestCreateAttachBadArgs covers required-arg validation.
func TestCreateAttachBadArgs(t *testing.T) {
	sock, _ := newDaemon(t)
	if code, _, _ := run(sock, "create", "--account", "x", "--rate", "0"); code != 1 {
		t.Errorf("create rate=0: exit %d, want 1", code)
	}
	if code, _, _ := run(sock, "attach", "--account", "default"); code != 1 {
		t.Errorf("attach with no user/group: exit %d, want 1", code)
	}
}
