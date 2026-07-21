package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// TestHandleLogRendersAccount confirms handleLog returns an account's WAL
// transitions, routed to the right account's state dir.
func TestHandleLogRendersAccount(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithRegistry(reg, testNow, Weights{})

	// Drive a couple of transitions on lab_smith via the kernel directly.
	smith, _ := reg.Resolve("lab_smith")
	smith.Submit("j1", 100, 1000)
	smith.Complete("j1", 40, 1100)

	resp := srv.handleLog(&wire.LogRequest{Account: "lab_smith"}, PeerCred{})
	if resp.LogResp == nil || !resp.LogResp.OK {
		t.Fatalf("log rejected: %+v", resp.LogResp)
	}
	if resp.LogResp.Account != "lab_smith" {
		t.Errorf("account = %q, want lab_smith", resp.LogResp.Account)
	}
	if len(resp.LogResp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (submit, settle): %+v", len(resp.LogResp.Entries), resp.LogResp.Entries)
	}
	if resp.LogResp.Entries[0].Kind != "submit" || resp.LogResp.Entries[1].Kind != "settle:complete" {
		t.Errorf("kinds = %q,%q; want submit, settle:complete",
			resp.LogResp.Entries[0].Kind, resp.LogResp.Entries[1].Kind)
	}
	// lab_jones (no transitions) has an empty log, not smith's.
	jr := srv.handleLog(&wire.LogRequest{Account: "lab_jones"}, PeerCred{})
	if jr.LogResp == nil || !jr.LogResp.OK || len(jr.LogResp.Entries) != 0 {
		t.Errorf("lab_jones log should be empty and OK, got %+v", jr.LogResp)
	}
	_ = reg.Close()
}

// TestHandleLogUnknownAccount confirms an unconfigured account is rejected.
func TestHandleLogUnknownAccount(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	srv := NewWithRegistry(reg, testNow, Weights{})
	resp := srv.handleLog(&wire.LogRequest{Account: "ghost"}, PeerCred{})
	if resp.LogResp == nil || resp.LogResp.OK {
		t.Errorf("expected rejection for unknown account, got %+v", resp.LogResp)
	}
}

// TestHandleLogReadScoped confirms log honors read visibility: a non-admin peer
// cannot read a restricted account's log.
func TestHandleLogReadScoped(t *testing.T) {
	srv := newAdminServer(t) // lab_jones restricted to carol; admins={alice,ops}
	// mallory (uid 13) is neither admin nor a lab_jones member.
	resp := srv.handleLog(&wire.LogRequest{Account: "lab_jones"}, adminPeer(13))
	if resp.LogResp == nil || resp.LogResp.OK {
		t.Errorf("mallory should not read lab_jones log, got %+v", resp.LogResp)
	}
	// alice (admin) can.
	ar := srv.handleLog(&wire.LogRequest{Account: "lab_jones"}, adminPeer(10))
	if ar.LogResp == nil || !ar.LogResp.OK {
		t.Errorf("admin should read any log, got %+v", ar.LogResp)
	}
}
