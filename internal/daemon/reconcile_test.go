package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// gateAndBind gates a 1:1 job on lab_smith and binds it to a Slurm job id, so the
// escrow is STARTED and routable by job id — the state a reconcile acts on.
func gateAndBind(t *testing.T, srv *Server, jobid string) {
	t.Helper()
	g := srv.handleGate(&wire.GateRequest{Account: "lab_smith", Partition: "cloud", TimeLimit: 100, NTasks: 1})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("gate rejected: %+v", g.GateResp)
	}
	b := srv.handleBind(&wire.BindRequest{Token: g.GateResp.Token, JobID: jobid})
	if b.BindResp == nil || !b.BindResp.OK {
		t.Fatalf("bind %s: %+v", jobid, b.BindResp)
	}
}

// TestReconcileSweepsStartedOrphan: a started job absent from the live set is
// reclaimed; one present in the live set is kept.
func TestReconcileSweepsStartedOrphan(t *testing.T) {
	srv := newAdminServer(t) // lab_smith (1000), lab_jones (500); admins alice(10)/ops(11)
	smith, _ := srv.reg.Resolve("lab_smith")

	gateAndBind(t, srv, "100") // escrow 100, started
	gateAndBind(t, srv, "200") // escrow 100, started
	if smith.Balance() != 1000-200 {
		t.Fatalf("after two gates balance = %d, want 800", smith.Balance())
	}

	// squeue reports only job 100 still live → job 200 is an orphan.
	resp := srv.handleReconcile(&wire.ReconcileRequest{LiveJobIDs: []string{"100"}}, adminPeer(10))
	if resp.ReconcileResp == nil || !resp.ReconcileResp.OK {
		t.Fatalf("reconcile failed: %+v", resp.ReconcileResp)
	}
	if resp.ReconcileResp.Swept != 1 {
		t.Errorf("swept = %d, want 1 (job 200)", resp.ReconcileResp.Swept)
	}
	// Job 200's escrow was full-refunded (OrphanRefundFull); job 100 untouched.
	if smith.Balance() != 1000-100 {
		t.Errorf("balance = %d, want 900 (200 refunded, 100 still live)", smith.Balance())
	}
	if smith.Live() != 1 {
		t.Errorf("live escrows = %d, want 1 (job 100)", smith.Live())
	}
	if ok, _ := smith.ConservationOK(); !ok {
		t.Error("conservation broken after reconcile")
	}
}

// TestReconcileSkipsUnbound: a gated-but-never-bound escrow has no jobToToken
// entry and is NOT swept by reconcile (it belongs to the unbound-token janitor),
// even though its (nonexistent) job id isn't in the live set.
func TestReconcileSkipsUnbound(t *testing.T) {
	srv := newAdminServer(t)
	smith, _ := srv.reg.Resolve("lab_smith")
	// Gate without bind → started == false, no jobToToken entry.
	g := srv.handleGate(&wire.GateRequest{Account: "lab_smith", Partition: "cloud", TimeLimit: 100, NTasks: 1})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatal("gate rejected")
	}
	before := smith.Balance()

	resp := srv.handleReconcile(&wire.ReconcileRequest{LiveJobIDs: []string{}}, adminPeer(10))
	if resp.ReconcileResp == nil || !resp.ReconcileResp.OK {
		t.Fatalf("reconcile failed: %+v", resp.ReconcileResp)
	}
	if resp.ReconcileResp.Swept != 0 {
		t.Errorf("swept = %d, want 0 (unbound escrow is SweepUnbound's job)", resp.ReconcileResp.Swept)
	}
	if smith.Balance() != before || smith.Live() != 1 {
		t.Errorf("reconcile disturbed an unbound escrow: balance %d live %d", smith.Balance(), smith.Live())
	}
}

// TestReconcileRequiresAdmin: reconcile is mutating → non-admins rejected, no
// sweep.
func TestReconcileRequiresAdmin(t *testing.T) {
	srv := newAdminServer(t)
	gateAndBind(t, srv, "100")
	smith, _ := srv.reg.Resolve("lab_smith")
	before := smith.Balance()

	// mallory (13) is not an admin.
	resp := srv.handleReconcile(&wire.ReconcileRequest{LiveJobIDs: []string{}}, adminPeer(13))
	if resp.ReconcileResp == nil || resp.ReconcileResp.OK {
		t.Fatalf("non-admin reconcile should be rejected: %+v", resp.ReconcileResp)
	}
	if smith.Balance() != before {
		t.Errorf("rejected reconcile still swept: balance %d -> %d", before, smith.Balance())
	}
}

// TestReconcileEmptyLiveSweepsAll: with nothing live, every started escrow is an
// orphan and reclaimed.
func TestReconcileEmptyLiveSweepsAll(t *testing.T) {
	srv := newAdminServer(t)
	gateAndBind(t, srv, "100")
	gateAndBind(t, srv, "200")

	resp := srv.handleReconcile(&wire.ReconcileRequest{LiveJobIDs: []string{}}, adminPeer(0)) // root
	if resp.ReconcileResp == nil || !resp.ReconcileResp.OK {
		t.Fatalf("reconcile failed: %+v", resp.ReconcileResp)
	}
	if resp.ReconcileResp.Swept != 2 {
		t.Errorf("swept = %d, want 2 (both orphaned)", resp.ReconcileResp.Swept)
	}
	smith, _ := srv.reg.Resolve("lab_smith")
	if smith.Live() != 0 || smith.Balance() != 1000 {
		t.Errorf("both should be reclaimed: live %d balance %d", smith.Live(), smith.Balance())
	}
}
