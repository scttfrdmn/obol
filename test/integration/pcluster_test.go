//go:build integration

// Package pcluster_test drives the obol GATE seam against a real AWS
// ParallelCluster. It deploys obold + the Lua shim + prolog/epilog to the
// cluster head node over SSH, seeds a budget, and runs the sbatch lifecycle,
// asserting the budget moved correctly — the cloud counterpart to the local
// Docker tier (test/docker), on real multi-node Slurm.
//
// Guarded by the `integration` build tag so it never runs in the default suite.
// Run it with:  make integ-pcluster
//
// It reads all cluster coordinates from the environment and SKIPS with a clear
// message when they are unset, so `make integ-pcluster` is safe to run anywhere.
// It never provisions or destroys AWS resources — the cluster must already
// exist (honoring CLAUDE.md's no-destructive-AWS rule).
//
// Required environment:
//
//	OBOL_INTEG_CLUSTER   ParallelCluster name (presence enables the test)
//	OBOL_INTEG_HEAD      head-node host or IP for SSH
//	OBOL_INTEG_SSH_USER  SSH user (default: rocky)
//	OBOL_INTEG_SSH_KEY   path to the SSH private key
//
// Optional:
//
//	OBOL_INTEG_PARTITION cloud partition to submit to (default: serial-requeue)
//	OBOL_INTEG_ACCOUNT   Slurm account (default: obol_test)
//	OBOL_INTEG_BALANCE   seed balance (default: 100000)
//	OBOL_INTEG_RATE      cost rate units/sec (default: 1)
package pcluster_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// env captures the resolved cluster coordinates for a run.
type env struct {
	cluster, head, user, key string
	partition, account       string
	balance, rate            int
}

// loadEnv reads the environment, or skips the test when the cluster is not
// configured. Presence of OBOL_INTEG_CLUSTER is the enable switch.
func loadEnv(t *testing.T) env {
	t.Helper()
	cluster := os.Getenv("OBOL_INTEG_CLUSTER")
	if cluster == "" {
		t.Skip("OBOL_INTEG_CLUSTER unset; skipping AWS ParallelCluster integration test " +
			"(set OBOL_INTEG_CLUSTER/HEAD/SSH_KEY to run — see docs/INTEGRATION.md)")
	}
	head := os.Getenv("OBOL_INTEG_HEAD")
	key := os.Getenv("OBOL_INTEG_SSH_KEY")
	if head == "" || key == "" {
		t.Fatal("OBOL_INTEG_CLUSTER is set but OBOL_INTEG_HEAD and/or OBOL_INTEG_SSH_KEY are missing")
	}
	e := env{
		cluster:   cluster,
		head:      head,
		user:      envOr("OBOL_INTEG_SSH_USER", "rocky"),
		key:       key,
		partition: envOr("OBOL_INTEG_PARTITION", "serial-requeue"),
		account:   envOr("OBOL_INTEG_ACCOUNT", "obol_test"),
		balance:   envIntOr("OBOL_INTEG_BALANCE", 100000),
		rate:      envIntOr("OBOL_INTEG_RATE", 1),
	}
	return e
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ssh runs a command on the head node and returns combined output.
func (e env) ssh(t *testing.T, command string) (string, error) {
	t.Helper()
	args := []string{
		"-i", e.key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=15",
		fmt.Sprintf("%s@%s", e.user, e.head),
		command,
	}
	out, err := exec.Command("ssh", args...).CombinedOutput()
	return string(out), err
}

// mustSSH runs a command on the head node, failing the test on error.
func (e env) mustSSH(t *testing.T, command string) string {
	t.Helper()
	out, err := e.ssh(t, command)
	if err != nil {
		t.Fatalf("ssh %q failed: %v\n%s", command, err, out)
	}
	return out
}

// scp copies a local file to the head node.
func (e env) scp(t *testing.T, local, remote string) {
	t.Helper()
	args := []string{
		"-i", e.key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		local,
		fmt.Sprintf("%s@%s:%s", e.user, e.head, remote),
	}
	if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
		t.Fatalf("scp %s -> %s failed: %v\n%s", local, remote, err, out)
	}
}

var balanceRe = regexp.MustCompile(`Balance:\s+(\d+)\s*/`)

// showBalance reads `obol show` on the head node and parses the current balance.
func (e env) showBalance(t *testing.T) int {
	t.Helper()
	out := e.mustSSH(t, "obol --socket /run/obol/obold.sock show")
	m := balanceRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse balance from:\n%s", out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// repoRoot walks up from the test file to the module root (where go.mod lives).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

// deploySeam builds a linux obold/obol, uploads them plus the Lua seam and
// prolog/epilog to the head node, wires slurm.conf, and starts obold. It assumes
// passwordless sudo on the head node (the ParallelCluster default for the admin
// user). Returns a teardown func.
func (e env) deploySeam(t *testing.T) func() {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()

	// Build linux binaries matching the cluster arch (default amd64; override
	// with OBOL_INTEG_ARCH for Graviton clusters).
	arch := envOr("OBOL_INTEG_ARCH", "amd64")
	for _, bin := range []string{"obold", "obol"} {
		out := filepath.Join(tmp, bin)
		cmd := exec.Command("go", "build", "-o", out, "./cmd/"+bin)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", bin, err, b)
		}
		e.scp(t, out, "/tmp/"+bin)
	}

	// Upload the seam files.
	for _, f := range []struct{ local, remote string }{
		{"seam/lua/obol_wire.lua", "/tmp/obol_wire.lua"},
		{"seam/lua/obol_transport.lua", "/tmp/obol_transport.lua"},
		{"seam/lua/job_submit.lua", "/tmp/job_submit.lua"},
		{"seam/slurm/obol-prolog.sh", "/tmp/obol-prolog.sh"},
		{"seam/slurm/obol-epilog.sh", "/tmp/obol-epilog.sh"},
	} {
		e.scp(t, filepath.Join(root, f.local), f.remote)
	}

	// Install everything and start obold. One combined remote script keeps the
	// round-trips down; each step is idempotent.
	install := strings.Join([]string{
		"set -e",
		"sudo install -m0755 /tmp/obold /tmp/obol /usr/local/bin/",
		"sudo install -d -m0755 /etc/slurm/lua /run/obol",
		"sudo install -m0644 /tmp/obol_wire.lua /tmp/obol_transport.lua /etc/slurm/lua/",
		"sudo install -m0644 /tmp/job_submit.lua /etc/slurm/job_submit.lua",
		"sudo install -m0755 /tmp/obol-prolog.sh /tmp/obol-epilog.sh /usr/local/bin/",
		// Start obold (fresh budget seeded per run).
		"sudo pkill obold || true",
		"sudo rm -f /run/obol/obold.sock /var/lib/obol/*.log /var/lib/obol/*.json || true",
		fmt.Sprintf("sudo nohup obold -socket /run/obol/obold.sock -state-dir /var/lib/obol "+
			"-create -balance %d -rate %d -sync=false >/tmp/obold.log 2>&1 &", e.balance, e.rate),
		"sleep 2",
		"sudo chmod 666 /run/obol/obold.sock",
		// Wire the plugin + prolog/epilog into slurm.conf if not already present.
		"grep -q '^JobSubmitPlugins=lua' /etc/slurm/slurm.conf || echo 'JobSubmitPlugins=lua' | sudo tee -a /etc/slurm/slurm.conf",
		"grep -q 'obol-prolog' /etc/slurm/slurm.conf || echo 'Prolog=/usr/local/bin/obol-prolog.sh' | sudo tee -a /etc/slurm/slurm.conf",
		"grep -q 'obol-epilog' /etc/slurm/slurm.conf || echo 'Epilog=/usr/local/bin/obol-epilog.sh' | sudo tee -a /etc/slurm/slurm.conf",
		"sudo scontrol reconfigure",
		"sleep 2",
	}, " && ")
	e.mustSSH(t, install)

	return func() {
		// Teardown: stop obold and remove the plugin lines, then reconfigure.
		_, _ = e.ssh(t, strings.Join([]string{
			"sudo pkill obold || true",
			"sudo sed -i '/JobSubmitPlugins=lua/d;/obol-prolog/d;/obol-epilog/d' /etc/slurm/slurm.conf",
			"sudo scontrol reconfigure || true",
		}, " ; "))
	}
}

// TestPClusterGateLifecycle deploys the seam and runs the funded lifecycle,
// unfunded rejection, and token-stamp assertions against real multi-node Slurm.
func TestPClusterGateLifecycle(t *testing.T) {
	e := loadEnv(t)
	teardown := e.deploySeam(t)
	t.Cleanup(teardown)

	// Ensure the test account exists in the cluster's accounting.
	_, _ = e.ssh(t, fmt.Sprintf("sudo sacctmgr -i add account %s Organization=obol 2>/dev/null; "+
		"sudo sacctmgr -i add user $USER Account=%s 2>/dev/null", e.account, e.account))

	before := e.showBalance(t)

	// Funded: a 1-minute job escrows rate*60 units.
	sub := e.mustSSH(t, fmt.Sprintf(
		"sbatch --parsable --account=%s --partition=%s --time=1 --wrap='sleep 5'",
		e.account, e.partition))
	jobid := strings.TrimSpace(lastLine(sub))
	if jobid == "" {
		t.Fatalf("sbatch returned no job id:\n%s", sub)
	}
	wantEscrow := e.rate * 60
	if after := e.showBalance(t); after != before-wantEscrow {
		t.Errorf("after submit balance = %d, want %d (escrow %d)", after, before-wantEscrow, wantEscrow)
	}

	// Token stamped into admin_comment (admin_comment writability on this gen).
	ac := e.mustSSH(t, "scontrol show job "+jobid+" 2>/dev/null | grep -io 'AdminComment=budget:[a-f0-9]*' || true")
	if !strings.Contains(ac, "AdminComment=budget:") {
		t.Errorf("job %s missing budget token in admin_comment: %q", jobid, ac)
	}

	// Wait for completion + epilog settle.
	e.waitJobGone(t, jobid)
	time.Sleep(4 * time.Second)

	show := e.mustSSH(t, "obol --socket /run/obol/obold.sock show")
	if !strings.Contains(show, "Conservation:  OK") {
		t.Errorf("conservation not OK after lifecycle:\n%s", show)
	}
	if after := e.showBalance(t); after <= before-wantEscrow {
		t.Errorf("balance did not recover after settle: %d", after)
	}

	// Unfunded: a job far exceeding the balance is rejected at submit.
	_, err := e.ssh(t, fmt.Sprintf(
		"sbatch --parsable --account=%s --partition=%s --time=100000 --wrap='true'",
		e.account, e.partition))
	if err == nil {
		t.Error("expected an unfundable job to be rejected at submit")
	}
}

func (e env) waitJobGone(t *testing.T, jobid string) {
	t.Helper()
	for i := 0; i < 60; i++ {
		out, _ := e.ssh(t, "squeue -h -j "+jobid+" -o %T 2>/dev/null || true")
		if strings.TrimSpace(out) == "" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("job %s did not leave the queue", jobid)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
