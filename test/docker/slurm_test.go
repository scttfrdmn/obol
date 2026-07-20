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

func showBalance(t *testing.T) int {
	t.Helper()
	out := dexec(t, `obol --socket /run/obol/obold.sock show`)
	m := balanceRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse balance from:\n%s", out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
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
	before := showBalance(t)

	// 1-minute job at rate 1 => 60 units escrowed at submit.
	out := dexec(t, `sbatch --parsable --account=lab --partition=cloud --time=1 --wrap="sleep 3"`)
	jobid := strings.TrimSpace(out)
	if jobid == "" {
		t.Fatalf("sbatch returned no job id: %s", out)
	}

	// After submit the escrow should have debited the balance.
	afterSubmit := showBalance(t)
	if afterSubmit != before-60 {
		t.Errorf("after submit balance = %d, want %d (escrow 60)", afterSubmit, before-60)
	}

	waitJobGone(t, jobid)
	time.Sleep(2 * time.Second) // let the epilog settle

	// After settle: only the ~3s runtime is consumed, the tail refunded, so the
	// balance is close to `before` (within the few seconds actually run).
	afterSettle := showBalance(t)
	if afterSettle <= afterSubmit {
		t.Errorf("after settle balance = %d, expected refund above %d", afterSettle, afterSubmit)
	}
	if afterSettle > before {
		t.Errorf("after settle balance = %d exceeds original %d (over-refund)", afterSettle, before)
	}

	// Conservation must hold.
	show := dexec(t, `obol --socket /run/obol/obold.sock show`)
	if !strings.Contains(show, "Conservation:  OK") {
		t.Errorf("conservation not OK:\n%s", show)
	}
}

// TestUnfundedJobRejected: a job whose cost exceeds the balance is rejected at
// submit by the gate, and nothing is escrowed.
func TestUnfundedJobRejected(t *testing.T) {
	before := showBalance(t)
	// time=100000 min * 60 = 6,000,000 units >> balance. Expect sbatch to fail.
	out, err := exec.Command("docker", "exec", container, "bash", "-lc",
		`sbatch --parsable --account=lab --partition=cloud --time=100000 --wrap="true"`).CombinedOutput()
	if err == nil {
		t.Errorf("expected sbatch to fail for an unfundable job, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "obol") && !strings.Contains(string(out), "budget") {
		t.Logf("rejection message (informational): %s", out)
	}
	if after := showBalance(t); after != before {
		t.Errorf("balance changed on a rejected job: %d -> %d", before, after)
	}
}

// TestMultiTenant submits jobs as multiple users across multiple groups (Slurm
// accounts) concurrently, and asserts the gate handles the mix and conservation
// holds once everything settles. The obold MVP is single-budget, so this proves
// correct multi-user/multi-account identity plumbing and gate correctness under
// a realistic multi-tenant pattern — the fixture the per-group hierarchy work
// (#17/#18) will later split into distinct budgets.
func TestMultiTenant(t *testing.T) {
	before := showBalance(t)

	// (user, account) pairs: two groups, two users each.
	subs := []struct{ user, account string }{
		{"alice", "lab_smith"},
		{"bob", "lab_smith"},
		{"carol", "lab_jones"},
		{"dave", "lab_jones"},
	}

	// Use longer jobs so all four are demonstrably escrowed at once before any
	// settles — this is what lets us observe the concurrent-escrow low-water mark
	// deterministically rather than racing the fast settles.
	var jobids []string
	for _, s := range subs {
		// sudo -u runs sbatch as the target user, so the gate sees a real uid and
		// the job is attributed to that user's account.
		out := dexec(t, `sudo -u `+s.user+` sbatch --parsable --account=`+s.account+
			` --partition=cloud --time=1 --wrap="sleep 15"`)
		jobid := strings.TrimSpace(lastLine(out))
		if jobid == "" || !isNumeric(jobid) {
			t.Fatalf("sbatch as %s/%s returned no job id: %q", s.user, s.account, out)
		}
		jobids = append(jobids, jobid)
	}

	// While all four (1-minute) jobs are live, 4*60 = 240 units are escrowed.
	// Poll briefly for the balance to reach that low-water mark — jobs run 15s,
	// so the window is wide.
	wantLow := before - 240
	reached := false
	for i := 0; i < 10; i++ {
		if showBalance(t) <= wantLow {
			reached = true
			break
		}
		time.Sleep(time.Second)
	}
	if !reached {
		t.Errorf("balance never reached the 4×60 escrow low-water mark %d (got %d)", wantLow, showBalance(t))
	}
	// Conservation must hold while all four tenants' jobs are live.
	if show := dexec(t, `obol --socket /run/obol/obold.sock show`); !strings.Contains(show, "Conservation:  OK") {
		t.Errorf("conservation not OK mid-flight:\n%s", show)
	}

	for _, j := range jobids {
		waitJobGone(t, j)
	}
	time.Sleep(3 * time.Second) // let all epilogs settle

	// All settled: conservation holds, no live escrows, balance recovered.
	show := dexec(t, `obol --socket /run/obol/obold.sock show`)
	if !strings.Contains(show, "Conservation:  OK") {
		t.Errorf("conservation not OK after multi-tenant mix:\n%s", show)
	}
	if !strings.Contains(show, "Live:          0 escrows") {
		t.Errorf("expected all escrows settled:\n%s", show)
	}
	if after := showBalance(t); after <= wantLow {
		t.Errorf("balance did not recover after settle: low=%d final=%d", wantLow, after)
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
