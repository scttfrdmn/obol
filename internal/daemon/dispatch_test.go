package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// burstDispatchServer builds a server over one burst-enabled account (r0 =
// 100000/1000000 ... = small; use a tight window so r0 is meaningful). c=1/s,
// B0=100000, window [now, now+1000) via a 1000s window so r0 = 100/s.
func burstDispatchServer(t *testing.T) *Server {
	t.Helper()
	cfg := &Config{Accounts: []AccountConfig{
		{Name: "burstlab", Balance: 100000, Rate: 1, Window: "1000s",
			BurstEnabled: true, BurstCeilingPct: 1.0},
	}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return NewWithRegistry(reg, testNow, Weights{})
}

// TestHandleDispatchBurstEnabled: with burst on, a job that fits banked headroom
// dispatches; one whose reservation exceeds the pot holds.
func TestHandleDispatchBurstEnabled(t *testing.T) {
	srv := burstDispatchServer(t)
	// testNow()=1000; account window is [1000, 2000), r0 = 100000/1000 = 100/s.
	bd, _ := srv.reg.Resolve("burstlab")

	// Drive a background job so aggregate burn is at r0, then a second job needs
	// burst tokens. First push rLive to r0 with a job at C=100.
	if err := bd.SubmitAt("bg", 100, 1000, testNow()); err != nil {
		t.Fatal(err)
	}
	if err := bd.Start("bg", testNow()); err != nil {
		t.Fatal(err)
	}
	// Nothing banked yet (running exactly at r0 banks nothing), so a job pushing
	// over r0 must hold.
	r := srv.handleDispatch(&wire.DispatchRequest{Account: "burstlab", TimeLimit: 100}, PeerCred{}).DispatchResp
	if r == nil || !r.OK {
		t.Fatalf("dispatch failed: %+v", r)
	}
	if r.Dispatch {
		t.Errorf("with no banked pot an over-r0 job should hold: %+v", r)
	}
	if r.Hold == "" {
		t.Errorf("expected a hold reason, got %+v", r)
	}
}

// TestHandleDispatchUnderR0: a small job that keeps burn at/under r0 needs no
// tokens and dispatches immediately.
func TestHandleDispatchUnderR0(t *testing.T) {
	srv := burstDispatchServer(t)
	// No running jobs → rLive 0; a job at the flat rate 1/s for 100s is far under
	// r0=100/s, reserves nothing.
	r := srv.handleDispatch(&wire.DispatchRequest{Account: "burstlab", TimeLimit: 100}, PeerCred{}).DispatchResp
	if r == nil || !r.OK || !r.Dispatch {
		t.Fatalf("under-r0 job should dispatch: %+v", r)
	}
	if r.Reserve != 0 {
		t.Errorf("under-r0 reserve = %d, want 0", r.Reserve)
	}
	if r.RateSource != "flat" || r.Rate != 1 {
		t.Errorf("rate resolution wrong: %+v", r)
	}
}

// TestHandleDispatchBurstDisabled: a non-burst account always dispatches.
func TestHandleDispatchBurstDisabled(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow) // no burst
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := NewWithRegistry(reg, testNow, Weights{})
	r := srv.handleDispatch(&wire.DispatchRequest{Account: "lab_smith", TimeLimit: 1000000}, PeerCred{}).DispatchResp
	if r == nil || !r.OK || !r.Dispatch {
		t.Fatalf("burst-disabled account should always dispatch: %+v", r)
	}
}

// TestHandleDispatchRateResolution: node-type worst-case rate feeds MayDispatch,
// exposed via RateSource (mirrors the simulate handler test).
func TestHandleDispatchRateResolution(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{{Name: "lab", Balance: 100000, Rate: 1, Window: "1000s",
			BurstEnabled: true, BurstCeilingPct: 1.0}},
		NodeTypes:  map[string]NodeRate{"spr": {Rate: 10}, "icx": {Rate: 6}},
		Partitions: []PartitionConfig{{Name: "priced", NodeTypes: []string{"spr", "icx"}}},
	}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := NewWithRegistry(reg, testNow, Weights{})
	nc, _ := BuildNodeCost(cfg)
	srv.SetNodeCost(nc)

	r := srv.handleDispatch(&wire.DispatchRequest{Account: "lab", Partition: "priced", TimeLimit: 100}, PeerCred{}).DispatchResp
	if r == nil || !r.OK {
		t.Fatalf("dispatch failed: %+v", r)
	}
	if r.RateSource != "node-type worst-case" || r.Rate != 10 {
		t.Errorf("rate resolution = %d/%q, want 10/node-type worst-case", r.Rate, r.RateSource)
	}
}

// TestHandleDispatchUnknownAccount: an unknown account is a clean reject.
func TestHandleDispatchUnknownAccount(t *testing.T) {
	srv := burstDispatchServer(t)
	r := srv.handleDispatch(&wire.DispatchRequest{Account: "ghost", TimeLimit: 100}, PeerCred{}).DispatchResp
	if r == nil || r.OK {
		t.Fatalf("unknown account should reject: %+v", r)
	}
}

// TestHandleDispatchReadScoping: a restricted account is not visible to a
// non-member peer (same visibility rule as show/simulate).
func TestHandleDispatchReadScoping(t *testing.T) {
	srv := newAdminServer(t) // lab_jones restricted to carol; admins alice(10)/ops(11)
	// mallory (13) is not a member of lab_jones → dispatch query denied.
	r := srv.handleDispatch(&wire.DispatchRequest{Account: "lab_jones", TimeLimit: 100}, adminPeer(13)).DispatchResp
	if r == nil || r.OK {
		t.Fatalf("restricted account should be hidden from non-member: %+v", r)
	}
	// carol (12) is a member → allowed (burst disabled on that account → dispatch).
	r = srv.handleDispatch(&wire.DispatchRequest{Account: "lab_jones", TimeLimit: 100}, adminPeer(12)).DispatchResp
	if r == nil || !r.OK || !r.Dispatch {
		t.Fatalf("member should get a dispatch verdict: %+v", r)
	}
}
