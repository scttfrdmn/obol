//go:build docker_integration

// Package docker_test drives the obol GATE seam against a real single-node Slurm
// (22.05) running in a container. It builds the image (which bakes in the shim,
// prolog/epilog, and the obold/obol binaries), boots it, submits jobs via
// sbatch, and asserts the budget moved correctly — the full gate→escrow→run→
// settle→refund path against an actual slurmctld.
//
// Guarded by the docker_integration build tag so it never runs in the default
// suite. Run it with:  make integ-docker   (or: go test -tags=docker_integration ./test/docker/)
// It skips cleanly when Docker is unavailable.
package docker_test

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	image     = "obol-slurm-test:latest"
	container = "obol-integ"
)

// sh runs a command, failing the test on error, and returns combined output.
func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s\nerr: %v\nout: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// dexec runs a command inside the running container via `docker exec`.
func dexec(t *testing.T, script string) string {
	t.Helper()
	return sh(t, "docker", "exec", container, "bash", "-lc", script)
}

// lastLine returns the last non-empty line of s (sbatch may print warnings
// before the parsable job id).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

// isNumeric reports whether s is all digits (a Slurm job id).
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// balanceRe extracts the "Balance: N / M" current balance from `obol show`.
var balanceRe = regexp.MustCompile(`Balance:\s+(\d+)\s*/`)

// showBalance reads the current balance of an account's budget (the daemon runs
// multi-account, so a specific account must be named).
func showBalance(t *testing.T, account string) int {
	t.Helper()
	out := dexec(t, `obol --socket /run/obol/obold.sock show --account `+account)
	m := balanceRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse balance for %s from:\n%s", account, out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// showAccount returns the full `obol show --account` output for assertions.
func showAccount(t *testing.T, account string) string {
	t.Helper()
	return dexec(t, `obol --socket /run/obol/obold.sock show --account `+account)
}

// TestMain builds the image and boots the container once for all subtests, then
// tears it down. Docker-absent => skip the whole package.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		// No docker: skip by running nothing. testing reports "no tests to run".
		return
	}
	// Best-effort cleanup of a stale container from a prior run.
	_ = exec.Command("docker", "rm", "-f", container).Run()

	// The image build needs the linux binaries staged; the Makefile target does
	// that before invoking go test. Here we just build the image from context.
	build := exec.Command("docker", "build", "-f", "Dockerfile.slurm", "-t", image, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		panic("docker build failed: " + err.Error() + "\n" + string(out))
	}
	run := exec.Command("docker", "run", "-d", "--name", container, "--privileged", image)
	if out, err := run.CombinedOutput(); err != nil {
		panic("docker run failed: " + err.Error() + "\n" + string(out))
	}
	// Wait for readiness (entrypoint touches /run/obol/ready, then node idle).
	ready := false
	for i := 0; i < 90; i++ {
		if exec.Command("docker", "exec", container, "test", "-f", "/run/obol/ready").Run() == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	code := 1
	if ready {
		time.Sleep(4 * time.Second) // let the node reach idle
		code = m.Run()
	}
	_ = exec.Command("docker", "rm", "-f", container).Run()
	if !ready {
		panic("container never became ready")
	}
	// os.Exit via the standard testing exit code.
	if code != 0 {
		panic("subtests failed")
	}
}

// waitJobGone polls until the job leaves the queue (completed/failed).
func waitJobGone(t *testing.T, jobid string) {
	t.Helper()
	for i := 0; i < 30; i++ {
		out := dexec(t, `squeue -h -j `+jobid+` -o "%T" 2>/dev/null || true`)
		if strings.TrimSpace(out) == "" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("job %s did not leave the queue", jobid)
}

// TestFundedJobLifecycle: a funded job gates (escrow), runs, and settles with
// the tail refunded — the full money path. Conservation holds throughout.
func TestFundedJobLifecycle(t *testing.T) {
	before := showBalance(t, "lab")

	// 1-minute job at rate 1 => 60 units escrowed at submit.
	out := dexec(t, `sbatch --parsable --account=lab --partition=cloud --time=1 --wrap="sleep 3"`)
	jobid := strings.TrimSpace(out)
	if jobid == "" {
		t.Fatalf("sbatch returned no job id: %s", out)
	}

	// After submit the escrow should have debited the balance.
	afterSubmit := showBalance(t, "lab")
	if afterSubmit != before-60 {
		t.Errorf("after submit balance = %d, want %d (escrow 60)", afterSubmit, before-60)
	}

	waitJobGone(t, jobid)
	time.Sleep(3 * time.Second) // let the controller-side jobcomp feed settle

	// After settle: only the ~3s runtime is consumed, the tail refunded, so the
	// balance is close to `before` (within the few seconds actually run).
	// Settlement here is driven by the jobcomp/script feed (#13) — no epilog is
	// installed in this image, so a refund here proves the controller-side path.
	afterSettle := showBalance(t, "lab")
	if afterSettle <= afterSubmit {
		t.Errorf("after settle balance = %d, expected refund above %d (jobcomp feed)", afterSettle, afterSubmit)
	}
	if afterSettle > before {
		t.Errorf("after settle balance = %d exceeds original %d (over-refund)", afterSettle, before)
	}

	// Conservation must hold.
	show := showAccount(t, "lab")
	if !strings.Contains(show, "Conservation:  OK") {
		t.Errorf("conservation not OK:\n%s", show)
	}
	// No live escrows means the job was fully settled by jobcomp.
	if !strings.Contains(show, "Live:          0 escrows") {
		t.Errorf("job not settled by jobcomp feed:\n%s", show)
	}
}

// TestUnfundedJobRejected: a job whose cost exceeds the balance is rejected at
// submit by the gate, and nothing is escrowed.
func TestUnfundedJobRejected(t *testing.T) {
	before := showBalance(t, "lab")
	// time=100000 min * 60 = 6,000,000 units >> balance. Expect sbatch to fail.
	out, err := exec.Command("docker", "exec", container, "bash", "-lc",
		`sbatch --parsable --account=lab --partition=cloud --time=100000 --wrap="true"`).CombinedOutput()
	if err == nil {
		t.Errorf("expected sbatch to fail for an unfundable job, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "obol") && !strings.Contains(string(out), "budget") {
		t.Logf("rejection message (informational): %s", out)
	}
	if after := showBalance(t, "lab"); after != before {
		t.Errorf("balance changed on a rejected job: %d -> %d", before, after)
	}
}

// TestMultiAccountIsolation proves a job under one account debits only THAT
// account's budget, leaving the other untouched — the payoff of per-account
// budgets (#18). obold runs with a real -config: lab_smith and lab_jones as
// independent pots. We observe the escrow at submit (which is deterministic —
// escrow happens at gate time, independent of whether the job later runs) rather
// than racing job execution, which is what makes this robust on a busy 1-node
// container. Jobs are submitted as root (launches cleanly here); the gate sees
// the account regardless of which user submits.
func TestMultiAccountIsolation(t *testing.T) {
	smithBefore := showBalance(t, "lab_smith")
	jonesBefore := showBalance(t, "lab_jones")

	// Gate a job under lab_smith. Escrow = rate(1) * 60s = 60. Hold it (don't wait
	// for completion) and check the debit landed on smith only.
	out := dexec(t, `sbatch --parsable --account=lab_smith --partition=cloud --time=1 --wrap="sleep 20"`)
	sjob := strings.TrimSpace(lastLine(out))
	if !isNumeric(sjob) {
		t.Fatalf("lab_smith sbatch returned no job id: %q", out)
	}

	// Poll briefly for smith's escrow to land (gate is synchronous, but sbatch →
	// balance visibility is a hair async through the socket).
	deb := false
	for i := 0; i < 10; i++ {
		if showBalance(t, "lab_smith") == smithBefore-60 {
			deb = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !deb {
		t.Errorf("lab_smith not debited by its own job: got %d, want %d", showBalance(t, "lab_smith"), smithBefore-60)
	}
	// lab_jones is completely untouched by a lab_smith submission — the isolation
	// property.
	if j := showBalance(t, "lab_jones"); j != jonesBefore {
		t.Errorf("lab_jones balance changed by a lab_smith job: %d -> %d", jonesBefore, j)
	}
	// Each account conserves independently.
	for _, acct := range []string{"lab_smith", "lab_jones"} {
		if s := showAccount(t, acct); !strings.Contains(s, "Conservation:  OK") {
			t.Errorf("%s conservation not OK:\n%s", acct, s)
		}
	}

	// Let the job settle; smith recovers, jones still untouched.
	waitJobGone(t, sjob)
	time.Sleep(3 * time.Second)
	if s := showBalance(t, "lab_smith"); s <= smithBefore-60 {
		t.Errorf("lab_smith did not recover after settle: %d", s)
	}
	if j := showBalance(t, "lab_jones"); j != jonesBefore {
		t.Errorf("lab_jones balance moved: %d != %d", j, jonesBefore)
	}
}

// TestAccessRejected proves the optional per-account allow-list: lab_jones is
// restricted to carol/dave, so alice (a lab_smith user) submitting to lab_jones
// is rejected for access — even though the job would otherwise be funded.
func TestAccessRejected(t *testing.T) {
	jonesBefore := showBalance(t, "lab_jones")
	out, err := exec.Command("docker", "exec", container, "bash", "-lc",
		`sudo -u alice sbatch --parsable --account=lab_jones --partition=cloud --time=1 --wrap="true"`).CombinedOutput()
	if err == nil {
		t.Errorf("expected alice's submit to lab_jones to be rejected for access, got:\n%s", out)
	}
	// Nothing escrowed on the rejected access.
	if after := showBalance(t, "lab_jones"); after != jonesBefore {
		t.Errorf("lab_jones balance changed on an access-rejected job: %d -> %d", jonesBefore, after)
	}
}

// TestNoBudgetRejected proves a submission to an account with no configured
// budget is rejected (SEAM §9: none resolves -> reject).
func TestNoBudgetRejected(t *testing.T) {
	// Register an ungated account in Slurm, then submit to it.
	dexec(t, `sudo sacctmgr -i add account ungated Organization=obol 2>/dev/null; sudo sacctmgr -i add user root Account=ungated 2>/dev/null; true`)
	out, err := exec.Command("docker", "exec", container, "bash", "-lc",
		`sbatch --parsable --account=ungated --partition=cloud --time=1 --wrap="true"`).CombinedOutput()
	if err == nil {
		t.Errorf("expected submit to an account with no budget to be rejected, got:\n%s", out)
	}
}

// TestGatedTokenStamped: an admitted job carries the budget token in
// admin_comment — proving admin_comment is writable on this Slurm generation
// (SEAM_DESIGN §13 gap #1).
func TestGatedTokenStamped(t *testing.T) {
	out := dexec(t, `sbatch --parsable --account=lab --partition=cloud --time=1 --wrap="sleep 1"`)
	jobid := strings.TrimSpace(out)
	ac := dexec(t, `scontrol show job `+jobid+` 2>/dev/null | grep -io "AdminComment=budget:[a-f0-9]*" || true`)
	if !strings.Contains(ac, "AdminComment=budget:") {
		t.Errorf("job %s has no budget token in admin_comment: %q", jobid, ac)
	}
	waitJobGone(t, jobid)
}

// TestTopUpAsAdmin: root (an admin in the config) tops up lab_smith; the balance
// and allocation grow and conservation holds. The daemon reads root's real uid
// via SO_PEERCRED over the socket.
func TestTopUpAsAdmin(t *testing.T) {
	before := showBalance(t, "lab_smith")
	out := dexec(t, `obol --socket /run/obol/obold.sock topup --account lab_smith --amount 25000`)
	if !strings.Contains(out, "ok:") {
		t.Fatalf("admin topup did not succeed: %q", out)
	}
	if after := showBalance(t, "lab_smith"); after != before+25000 {
		t.Errorf("balance after topup = %d, want %d", after, before+25000)
	}
	if s := showAccount(t, "lab_smith"); !strings.Contains(s, "Conservation:  OK") {
		t.Errorf("conservation not OK after topup:\n%s", s)
	}
}

// TestTopUpAsNonAdminRejected: a non-admin user (alice) is rejected by peer-cred
// even though she can reach the socket. Proves authz is on the kernel-verified
// uid, not the socket alone.
func TestTopUpAsNonAdminRejected(t *testing.T) {
	before := showBalance(t, "lab_smith")
	out, err := exec.Command("docker", "exec", container, "bash", "-lc",
		`sudo -u alice obol --socket /run/obol/obold.sock topup --account lab_smith --amount 999`).CombinedOutput()
	if err == nil {
		t.Errorf("expected non-admin topup to be rejected, got success:\n%s", out)
	}
	if after := showBalance(t, "lab_smith"); after != before {
		t.Errorf("balance changed on a rejected topup: %d -> %d", before, after)
	}
}

// TestListAsAdmin: root lists all accounts.
func TestListAsAdmin(t *testing.T) {
	out := dexec(t, `obol --socket /run/obol/obold.sock list`)
	for _, want := range []string{"lab_smith", "lab_jones", "ACCOUNT", "BALANCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}
