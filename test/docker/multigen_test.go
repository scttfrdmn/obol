//go:build docker_multigen

// Package docker_test (multi-generation tier) drives the obol GATE seam against
// Slurm BUILT FROM SOURCE at each burstlab generation's exact version + base OS
// (#16). Unlike the packaged tier (slurm_test.go, one EPEL 22.05 image), this
// builds Slurm from the SchedMD tarball per generation, matching burstlab's
// packer AMIs (~/src/burstlab/ami/*.pkr.hcl): same versions, same cgroup/v2 build
// deps, same configure recipe. It resolves the SEAM_DESIGN §10 question — that the
// Lua job_desc field set (admin_comment read/write, tres/time_limit, site_factor)
// works per Slurm generation — by running the real money path on each.
//
// Separate build tag (docker_multigen) and make target (integ-docker-multigen) so
// the slow source builds (~10-20 min/image) never run in the default suite or the
// fast packaged tier. Select generations with OBOL_INTEG_GENS (comma list; default
// all defined). Run: make integ-docker-multigen [OBOL_INTEG_GENS=gen2]
package docker_test

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// generation is one burstlab generation: a base image + Slurm version built from
// source. Names/versions/bases match ~/src/burstlab/ami/*.pkr.hcl and the
// terraform/generations/genN-* directories.
type generation struct {
	name         string // gen2, etc. (matches burstlab)
	baseImage    string // docker base
	slurmVersion string // SchedMD tarball version
	crbRepo      string // "crb" (Rocky 9/10) | "powertools" (Rocky 8)
}

// generations is the multi-gen matrix. gen1-3 are burstlab's three Rocky
// generations, each built from source (names/versions/bases match
// ~/src/burstlab/terraform/generations/genN-* and ami/*.pkr.hcl). Rocky 8 carries
// -devel packages in "powertools"; Rocky 9/10 use "crb". Rocky 10's image lives on
// quay.io.
//
// "managed" is the AWS PCS / ParallelCluster target: Slurm 25.11, which AWS PCS
// ships and which a live ParallelCluster (Slurm 25.11.4) ran the obol seam on
// (#131). It's outside burstlab's gen set but is the version managed AWS Slurm
// deploys, so the seam is validated against it here too. Built on Rocky 9.
var generations = []generation{
	{name: "gen1", baseImage: "rockylinux:8", slurmVersion: "22.05.11", crbRepo: "powertools"},
	{name: "gen2", baseImage: "rockylinux:9", slurmVersion: "23.11.10", crbRepo: "crb"},
	{name: "gen3", baseImage: "quay.io/rockylinux/rockylinux:10", slurmVersion: "24.05.5", crbRepo: "crb"},
	{name: "managed", baseImage: "rockylinux:9", slurmVersion: "25.11.1", crbRepo: "crb"},
}

// selectedGenerations filters the matrix by OBOL_INTEG_GENS (comma list); empty =
// all defined.
func selectedGenerations(t *testing.T) []generation {
	want := strings.TrimSpace(os.Getenv("OBOL_INTEG_GENS"))
	if want == "" {
		return generations
	}
	set := map[string]bool{}
	for _, g := range strings.Split(want, ",") {
		set[strings.TrimSpace(g)] = true
	}
	var out []generation
	for _, g := range generations {
		if set[g.name] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		t.Fatalf("OBOL_INTEG_GENS=%q matched no known generations %v", want, genNames())
	}
	return out
}

func genNames() []string {
	n := make([]string, len(generations))
	for i, g := range generations {
		n[i] = g.name
	}
	return n
}

// TestGenerations builds, boots, and exercises each selected generation in turn.
// Each generation gets its own image + container so failures are isolated.
func TestGenerations(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	for _, g := range selectedGenerations(t) {
		g := g
		t.Run(g.name, func(t *testing.T) {
			image := "obol-slurm-src:" + g.name
			container := "obol-multigen-" + g.name

			buildGenerationImage(t, g, image)
			bootContainer(t, image, container)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })

			// Assert the Slurm version is exactly what we built (guards a stale image).
			if v := dexecMG(t, container, "sinfo --version"); !strings.Contains(v, g.slurmVersion) {
				t.Fatalf("running Slurm %q, want %s", strings.TrimSpace(v), g.slurmVersion)
			}
			assertFundedLifecycle(t, container)
		})
	}
}

// buildGenerationImage builds the source image for one generation. The host must
// have staged the linux obold/obol binaries into test/docker/bin/ (the make
// target does this). The build compiles Slurm — slow.
func buildGenerationImage(t *testing.T, g generation, image string) {
	t.Helper()
	t.Logf("building %s: Slurm %s from source on %s (this compiles Slurm, ~10-20 min)", g.name, g.slurmVersion, g.baseImage)
	build := exec.Command("docker", "build",
		"-f", "Dockerfile.slurm-src",
		"--build-arg", "BASE_IMAGE="+g.baseImage,
		"--build-arg", "SLURM_VERSION="+g.slurmVersion,
		"--build-arg", "ENABLE_CRB="+g.crbRepo,
		"-t", image, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build (%s) failed: %v\n%s", g.name, err, out)
	}
}

// bootContainer runs an image and waits for the entrypoint readiness marker plus
// the node reaching idle.
func bootContainer(t *testing.T, image, container string) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", container).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", container, "--privileged", image).CombinedOutput(); err != nil {
		t.Fatalf("docker run (%s) failed: %v\n%s", container, err, out)
	}
	ready := false
	for i := 0; i < 90; i++ {
		if exec.Command("docker", "exec", container, "test", "-f", "/run/obol/ready").Run() == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
		t.Fatalf("%s never became ready\n%s", container, logs)
	}
	// Wait for the node to reach idle so jobs actually dispatch. Newer Slurm
	// (25.11) registers the node a bit slower than 22.05/23.11, so allow 90s.
	for i := 0; i < 90; i++ {
		out, _ := exec.Command("docker", "exec", container, "sinfo", "-h", "-o", "%T").CombinedOutput()
		if strings.Contains(string(out), "idle") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s node never reached idle", container)
}

// assertFundedLifecycle runs the core money path on a generation: a funded job
// gates (escrow), runs, and settles with the tail refunded — the gate→escrow→run→
// settle→refund path against a real slurmctld of this version. Conservation holds.
// It also asserts the admin_comment token round-trip (the SEAM_DESIGN §10 concern):
// the shim wrote the token from slurm_job_submit on this Slurm version.
func assertFundedLifecycle(t *testing.T, container string) {
	t.Helper()
	before := showBalanceMG(t, container, "lab")

	// 1-minute job at rate 1 => 60 units escrowed at submit.
	jobid := strings.TrimSpace(dexecMG(t, container, `sbatch --parsable --account=lab --partition=cloud --time=1 --wrap="sleep 3"`))
	if jobid == "" {
		t.Fatal("sbatch returned no job id")
	}

	// admin_comment carries the token the shim stamped (§10: writable from
	// slurm_job_submit on this generation).
	ac := dexecMG(t, container, `scontrol show job `+jobid+` 2>/dev/null | grep -io "AdminComment=budget:[a-f0-9]*" | head -1`)
	if !strings.Contains(ac, "budget:") {
		t.Errorf("admin_comment missing the budget token on this Slurm version: %q", ac)
	}

	afterSubmit := showBalanceMG(t, container, "lab")
	if afterSubmit != before-60 {
		t.Errorf("after submit balance = %d, want %d (escrow 60)", afterSubmit, before-60)
	}

	// Wait for the job to leave the queue, then let the jobcomp feed settle.
	for i := 0; i < 30; i++ {
		out, _ := exec.Command("docker", "exec", container, "squeue", "-h", "-j", jobid, "-o", "%T").CombinedOutput()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		time.Sleep(time.Second)
	}
	time.Sleep(3 * time.Second)

	afterSettle := showBalanceMG(t, container, "lab")
	if afterSettle <= afterSubmit {
		t.Errorf("after settle balance = %d, want > %d (tail refunded)", afterSettle, afterSubmit)
	}
	if afterSettle > before {
		t.Errorf("after settle balance = %d exceeds start %d (over-refund)", afterSettle, before)
	}
	// Conservation must hold after the full path.
	if out := showAccountMG(t, container, "lab"); !strings.Contains(out, "Conservation:  OK") {
		t.Errorf("conservation not OK after lifecycle:\n%s", out)
	}
}

// --- small self-contained helpers (kept separate from the packaged tier so the
// proven docker_integration harness is untouched) ---

func dexecMG(t *testing.T, container, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "bash", "-lc", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s: %v\n%s", container, err, out)
	}
	return string(out)
}

var balanceReMG = regexp.MustCompile(`Balance:\s+(\d+)\s*/`)

func showAccountMG(t *testing.T, container, account string) string {
	t.Helper()
	return dexecMG(t, container, `obol --socket /run/obol/obold.sock show --account `+account)
}

func showBalanceMG(t *testing.T, container, account string) int {
	t.Helper()
	m := balanceReMG.FindStringSubmatch(showAccountMG(t, container, account))
	if m == nil {
		t.Fatalf("could not parse balance for %s", account)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
