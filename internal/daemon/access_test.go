package daemon

import (
	"fmt"
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// fakeIdentity is an injectable identityResolver for tests: it maps uids to
// (user, groups) without touching the OS.
type fakeIdentity struct {
	users  map[uint32]string
	groups map[uint32][]string
}

func (f fakeIdentity) lookup(uid uint32) (string, []string, error) {
	u, ok := f.users[uid]
	if !ok {
		return "", nil, fmt.Errorf("no such uid %d", uid)
	}
	return u, f.groups[uid], nil
}

// TestAuthorizeOpenAccount: an account with no allow-list admits anyone and does
// NOT invoke the identity resolver (the default trust-Slurm path).
func TestAuthorizeOpenAccount(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "open", Balance: 1000, Rate: 1, Window: "1000000s"}}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithRegistry(reg, testNow, Weights{})
	// A resolver that panics if called — proves the open path never looks up.
	srv.ident = panicIdentity{t}

	if ok, reason := srv.authorize("open", 1234); !ok {
		t.Errorf("open account should admit anyone, got reject: %s", reason)
	}
	_ = reg.Close()
}

type panicIdentity struct{ t *testing.T }

func (p panicIdentity) lookup(uint32) (string, []string, error) {
	p.t.Fatal("identity resolver called for an unrestricted account")
	return "", nil, nil
}

// TestAuthorizeRestricted: a restricted account admits a listed user or group
// member and rejects others.
func TestAuthorizeRestricted(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "lab_jones", Balance: 1000, Rate: 1, Window: "1000000s",
		AllowUsers: []string{"carol"}, AllowGroups: []string{"jones"},
	}}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.ident = fakeIdentity{
		users: map[uint32]string{
			10: "carol",   // on AllowUsers
			11: "dave",    // in group jones
			12: "mallory", // neither
		},
		groups: map[uint32][]string{
			10: {"jones"},
			11: {"jones"},
			12: {"others"},
		},
	}

	cases := []struct {
		uid  uint32
		want bool
	}{
		{10, true},  // listed user
		{11, true},  // group member
		{12, false}, // neither
		{99, false}, // uid the resolver doesn't know -> fail closed
	}
	for _, c := range cases {
		if ok, _ := srv.authorize("lab_jones", c.uid); ok != c.want {
			t.Errorf("authorize(lab_jones, uid=%d) = %v, want %v", c.uid, ok, c.want)
		}
	}
}

// TestGateRejectsUnauthorized: end-to-end, an unauthorized submitter to a
// restricted account is rejected at the gate before any escrow.
func TestGateRejectsUnauthorized(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "lab_jones", Balance: 1000, Rate: 1, Window: "1000000s",
		AllowUsers: []string{"carol"},
	}}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.ident = fakeIdentity{users: map[uint32]string{5: "mallory"}, groups: map[uint32][]string{5: {"x"}}}

	// Drive handleGate directly (serveReg uses a default resolver; here we need
	// the fake, so call the handler).
	resp := srv.handleGate(&wire.GateRequest{Account: "lab_jones", UID: 5, TimeLimit: 100, NTasks: 1})
	if resp.GateResp == nil || resp.GateResp.Allow {
		t.Fatalf("expected unauthorized reject, got %+v", resp.GateResp)
	}
	// Nothing escrowed.
	jones, _ := reg.Resolve("lab_jones")
	if jones.Balance() != 1000 {
		t.Errorf("balance changed on an unauthorized gate: %d", jones.Balance())
	}
	_ = reg.Close()
}
