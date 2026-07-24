package main

import (
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    os.FileMode
		wantErr bool
	}{
		{"0660", 0o660, false},
		{"660", 0o660, false},
		{"0600", 0o600, false},
		{"0777", 0o777, false},
		{"", 0, true},        // ParseUint("") fails
		{"999", 0, true},     // 9 isn't an octal digit
		{"1000", 0, true},    // > 0777
		{"garbage", 0, true}, // not a number
	}
	for _, c := range cases {
		got, err := parseMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseMode(%q): want error, got %o", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMode(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMode(%q) = %o, want %o", c.in, got, c.want)
		}
	}
}

func TestLookupGID(t *testing.T) {
	// A numeric gid passes straight through.
	if gid, err := lookupGID("0"); err != nil || gid != 0 {
		t.Errorf("lookupGID(\"0\") = %d, %v; want 0, nil", gid, err)
	}
	// The current user's primary group resolves by name.
	u, err := user.Current()
	if err != nil {
		t.Skip("cannot resolve current user")
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skip("cannot resolve current group name")
	}
	gid, err := lookupGID(g.Name)
	if err != nil {
		t.Fatalf("lookupGID(%q): %v", g.Name, err)
	}
	if strconv.Itoa(gid) != u.Gid {
		t.Errorf("lookupGID(%q) = %d, want %s", g.Name, gid, u.Gid)
	}
	// A nonsense group is an error.
	if _, err := lookupGID("no-such-group-xyzzy"); err == nil {
		t.Error("lookupGID(nonexistent): want error, got nil")
	}
}

// TestApplySocketPerms exercises the real path: listen on a Unix socket, then
// apply a mode and (no-op) group, and confirm the mode landed.
func TestApplySocketPerms(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Empty group + empty mode is a no-op and must not error.
	if err := applySocketPerms(sock, "", ""); err != nil {
		t.Fatalf("no-op applySocketPerms: %v", err)
	}

	// Apply a specific mode; confirm it lands (masking to the perm bits).
	if err := applySocketPerms(sock, "", "0660"); err != nil {
		t.Fatalf("applySocketPerms mode: %v", err)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o660 {
		t.Errorf("socket mode = %o, want 0660", got)
	}

	// A bad mode surfaces an error (and doesn't panic).
	if err := applySocketPerms(sock, "", "abc"); err == nil {
		t.Error("applySocketPerms with bad mode: want error")
	}

	// A bad group surfaces an error.
	if err := applySocketPerms(sock, "no-such-group-xyzzy", ""); err == nil {
		t.Error("applySocketPerms with bad group: want error")
	}
}
