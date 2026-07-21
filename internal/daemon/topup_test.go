package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// adminConfig: two accounts, admins = {alice or group ops}. Enforcement on.
func adminConfig() *Config {
	return &Config{
		Accounts: []AccountConfig{
			{Name: "lab_smith", Balance: 1000, Rate: 1, Window: "1000000s"},
			{Name: "lab_jones", Balance: 500, Rate: 1, Window: "1000000s",
				AllowUsers: []string{"carol"}},
		},
		AdminUsers:  []string{"alice"},
		AdminGroups: []string{"ops"},
	}
}

// newAdminServer builds a server over adminConfig with a fake identity resolver.
func newAdminServer(t *testing.T) *Server {
	t.Helper()
	reg, err := NewRegistry(adminConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.ident = fakeIdentity{
		users: map[uint32]string{
			10: "alice",   // admin (AllowUsers)
			11: "bob",     // in group ops -> admin
			12: "carol",   // not admin; member of lab_jones
			13: "mallory", // nobody
		},
		groups: map[uint32][]string{
			10: {"smith"},
			11: {"ops"},
			12: {"jones"},
			13: {"nope"},
		},
	}
	return srv
}

func adminPeer(uid uint32) PeerCred { return PeerCred{UID: uid, Available: true} }

// TestTopUpRequiresAdmin: only an admin peer may top up; others rejected before
// any balance change.
func TestTopUpRequiresAdmin(t *testing.T) {
	srv := newAdminServer(t)
	smith, _ := srv.reg.Resolve("lab_smith")

	// mallory (uid 13, not admin) -> rejected, no change.
	resp := srv.handleTopUp(&wire.TopUpRequest{Account: "lab_smith", Amount: 500}, adminPeer(13))
	if resp.TopUpResp == nil || resp.TopUpResp.OK {
		t.Fatalf("expected non-admin topup to be rejected, got %+v", resp.TopUpResp)
	}
	if smith.Balance() != 1000 {
		t.Errorf("balance changed on a rejected topup: %d", smith.Balance())
	}

	// alice (uid 10, admin user) -> allowed.
	resp = srv.handleTopUp(&wire.TopUpRequest{Account: "lab_smith", Amount: 500}, adminPeer(10))
	if resp.TopUpResp == nil || !resp.TopUpResp.OK {
		t.Fatalf("admin topup rejected: %+v", resp.TopUpResp)
	}
	if smith.Balance() != 1500 {
		t.Errorf("balance after admin topup = %d, want 1500", smith.Balance())
	}
	if ok, _ := smith.ConservationOK(); !ok {
		t.Error("conservation broken after topup")
	}
}

// TestTopUpAdminByGroup: a peer in an admin group may top up.
func TestTopUpAdminByGroup(t *testing.T) {
	srv := newAdminServer(t)
	// bob (uid 11) is in group "ops" (admin group).
	resp := srv.handleTopUp(&wire.TopUpRequest{Account: "lab_jones", Amount: 100}, adminPeer(11))
	if resp.TopUpResp == nil || !resp.TopUpResp.OK {
		t.Fatalf("group-admin topup rejected: %+v", resp.TopUpResp)
	}
}

// TestTopUpRootIsAdmin: uid 0 is always admin, even without being listed.
func TestTopUpRootIsAdmin(t *testing.T) {
	srv := newAdminServer(t)
	resp := srv.handleTopUp(&wire.TopUpRequest{Account: "lab_smith", Amount: 1}, adminPeer(0))
	if resp.TopUpResp == nil || !resp.TopUpResp.OK {
		t.Fatalf("root topup rejected: %+v", resp.TopUpResp)
	}
}

// TestTopUpPeerUnavailableFailsClosed: when peer creds can't be read and admin
// enforcement is on, a mutating verb fails closed.
func TestTopUpPeerUnavailableFailsClosed(t *testing.T) {
	srv := newAdminServer(t)
	resp := srv.handleTopUp(&wire.TopUpRequest{Account: "lab_smith", Amount: 1}, PeerCred{})
	if resp.TopUpResp == nil || resp.TopUpResp.OK {
		t.Fatalf("expected fail-closed when peer creds unavailable, got %+v", resp.TopUpResp)
	}
}

// TestReadScoping: a non-admin peer sees open accounts and their own, but not
// restricted accounts they don't belong to; admins see all.
func TestReadScoping(t *testing.T) {
	srv := newAdminServer(t)

	// mallory (uid 13): lab_smith is open -> visible; lab_jones is restricted to
	// carol -> not visible.
	if !srv.canRead("lab_smith", adminPeer(13)) {
		t.Error("open account lab_smith should be readable by anyone")
	}
	if srv.canRead("lab_jones", adminPeer(13)) {
		t.Error("mallory should NOT read restricted lab_jones")
	}
	// carol (uid 12) is a member of lab_jones -> visible.
	if !srv.canRead("lab_jones", adminPeer(12)) {
		t.Error("carol (member) should read lab_jones")
	}
	// alice (admin) sees everything.
	if !srv.canRead("lab_jones", adminPeer(10)) {
		t.Error("admin should read any account")
	}
}

// TestListFiltersByVisibility: list returns only the accounts the peer may see.
func TestListFiltersByVisibility(t *testing.T) {
	srv := newAdminServer(t)

	// mallory: only lab_smith (open).
	resp := srv.handleList(adminPeer(13))
	if resp.ListResp == nil || !resp.ListResp.OK {
		t.Fatal("list failed")
	}
	names := map[string]bool{}
	for _, a := range resp.ListResp.Accounts {
		names[a.Account] = true
	}
	if !names["lab_smith"] || names["lab_jones"] {
		t.Errorf("mallory list = %v, want only lab_smith", names)
	}
	// admin sees both.
	resp = srv.handleList(adminPeer(10))
	if len(resp.ListResp.Accounts) != 2 {
		t.Errorf("admin should see 2 accounts, got %d", len(resp.ListResp.Accounts))
	}
}

// TestAuthzOffWhenNoAdmins: with no admin list configured, enforcement is off —
// topup allowed for anyone (socket-perms model), reads open.
func TestAuthzOffWhenNoAdmins(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow) // no admins
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	srv := NewWithRegistry(reg, testNow, Weights{})

	// Even an unavailable peer can top up when enforcement is off.
	resp := srv.handleTopUp(&wire.TopUpRequest{Account: "lab_smith", Amount: 100}, PeerCred{})
	if resp.TopUpResp == nil || !resp.TopUpResp.OK {
		t.Fatalf("topup should be allowed when admin enforcement is off: %+v", resp.TopUpResp)
	}
	if !srv.canRead("lab_smith", PeerCred{}) {
		t.Error("reads should be open when enforcement is off")
	}
}

// TestSetRateWindowAdminGated confirms set-rate/set-window require admin (like
// topup) and route to the right account.
func TestSetRateWindowAdminGated(t *testing.T) {
	srv := newAdminServer(t) // admins = {alice(uid10), group ops(uid11)}; mallory=13
	smith, _ := srv.reg.Resolve("lab_smith")

	// Non-admin rejected, no change.
	if r := srv.handleSetRate(&wire.SetRateRequest{Account: "lab_smith", Rate: 9}, adminPeer(13)); r.AckResp == nil || r.AckResp.OK {
		t.Errorf("non-admin set-rate should be rejected, got %+v", r.AckResp)
	}
	if smith.Report(testNow()).C == 9 {
		t.Error("rate changed by a non-admin")
	}
	// Admin allowed.
	if r := srv.handleSetRate(&wire.SetRateRequest{Account: "lab_smith", Rate: 9}, adminPeer(10)); r.AckResp == nil || !r.AckResp.OK {
		t.Fatalf("admin set-rate rejected: %+v", r.AckResp)
	}
	if smith.Report(testNow()).C != 9 {
		t.Errorf("rate = %d, want 9 after admin set-rate", smith.Report(testNow()).C)
	}

	// set-window: admin allowed, non-admin rejected.
	if r := srv.handleSetWindow(&wire.SetWindowRequest{Account: "lab_smith", TS: 0, TE: 5_000_000}, adminPeer(13)); r.AckResp == nil || r.AckResp.OK {
		t.Errorf("non-admin set-window should be rejected")
	}
	if r := srv.handleSetWindow(&wire.SetWindowRequest{Account: "lab_smith", TS: 0, TE: 5_000_000}, adminPeer(11)); r.AckResp == nil || !r.AckResp.OK {
		t.Fatalf("group-admin set-window rejected: %+v", r.AckResp)
	}
	if smith.Report(testNow()).TE != 5_000_000 {
		t.Errorf("TE = %d, want 5000000", smith.Report(testNow()).TE)
	}

	// Unknown account rejected.
	if r := srv.handleSetRate(&wire.SetRateRequest{Account: "ghost", Rate: 1}, adminPeer(10)); r.AckResp == nil || r.AckResp.OK {
		t.Errorf("set-rate on unknown account should be rejected")
	}
}

// TestCreateAndAttachAdminGated confirms create/attach require admin and that
// attach actually changes the access verdict.
func TestCreateAndAttachAdminGated(t *testing.T) {
	srv := newAdminServer(t) // admins alice(10)/ops(11); mallory=13

	// Non-admin create rejected.
	if r := srv.handleCreate(&wire.CreateRequest{Account: "lab_x", Balance: 100, Rate: 1}, adminPeer(13)); r.AckResp == nil || r.AckResp.OK {
		t.Errorf("non-admin create should be rejected: %+v", r.AckResp)
	}
	if _, err := srv.reg.Resolve("lab_x"); err == nil {
		t.Error("lab_x should not exist after a rejected create")
	}
	// Admin create succeeds.
	if r := srv.handleCreate(&wire.CreateRequest{Account: "lab_x", Balance: 100, Rate: 1, Window: "1000000s"}, adminPeer(10)); r.AckResp == nil || !r.AckResp.OK {
		t.Fatalf("admin create rejected: %+v", r.AckResp)
	}
	if _, err := srv.reg.Resolve("lab_x"); err != nil {
		t.Errorf("lab_x should resolve after create: %v", err)
	}

	// Attach to lab_x (currently open) restricts it. Non-admin attach rejected first.
	if r := srv.handleAttach(&wire.AttachRequest{Account: "lab_x", Users: []string{"alice"}}, adminPeer(13)); r.AttachResp == nil || r.AttachResp.OK {
		t.Errorf("non-admin attach should be rejected: %+v", r.AttachResp)
	}
	if r := srv.handleAttach(&wire.AttachRequest{Account: "lab_x", Users: []string{"alice"}}, adminPeer(10)); r.AttachResp == nil || !r.AttachResp.OK {
		t.Fatalf("admin attach rejected: %+v", r.AttachResp)
	}
	// lab_x is now restricted to alice: alice(10) authorized, mallory(13) not.
	if ok, _ := srv.authorize("lab_x", 10); !ok {
		t.Error("alice should be authorized for lab_x after attach")
	}
	if ok, _ := srv.authorize("lab_x", 13); ok {
		t.Error("mallory should NOT be authorized for restricted lab_x")
	}
	// Detach alice -> lab_x open again.
	if r := srv.handleAttach(&wire.AttachRequest{Account: "lab_x", Users: []string{"alice"}, Detach: true}, adminPeer(10)); r.AttachResp == nil || !r.AttachResp.OK {
		t.Fatalf("detach rejected: %+v", r.AttachResp)
	}
	if ok, _ := srv.authorize("lab_x", 13); !ok {
		t.Error("lab_x should be open after detaching its only user")
	}
}
