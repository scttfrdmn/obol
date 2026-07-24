package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// applySocketPerms sets the listen socket's group and mode after it's created,
// so a non-root process (e.g. slurmctld, which runs the GATE shim as the "slurm"
// user) can connect. connect(2) on a Unix socket requires WRITE permission, so a
// root-owned 0755 socket is unreachable by slurmctld — the gate then fails
// "budget daemon unreachable". Grouping the socket to "slurm" with mode 0660 lets
// slurmctld connect while keeping it off-limits to everyone else.
//
// This widens who can send READ verbs and drive the gate/bind/settle lifecycle —
// exactly slurmctld's job. It does NOT widen admin: MUTATING verbs are still gated
// on the SO_PEERCRED peer identity (see internal/daemon/access.go), which the
// socket mode can't spoof.
//
// group is a group name or numeric gid ("" leaves ownership unchanged). mode is
// an octal string like "0660" ("" leaves the mode at whatever Listen created).
func applySocketPerms(path, group, mode string) error {
	if group != "" {
		gid, err := lookupGID(group)
		if err != nil {
			return fmt.Errorf("socket-group %q: %w", group, err)
		}
		// -1 uid leaves the owner unchanged; only the group moves.
		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("chown socket to gid %d: %w", gid, err)
		}
	}
	if mode != "" {
		m, err := parseMode(mode)
		if err != nil {
			return fmt.Errorf("socket-mode %q: %w", mode, err)
		}
		if err := os.Chmod(path, m); err != nil {
			return fmt.Errorf("chmod socket to %o: %w", m, err)
		}
	}
	return nil
}

// lookupGID resolves a group name to its gid, accepting a numeric gid directly
// (so a container without /etc/group entries can still pass a number).
func lookupGID(group string) (int, error) {
	if g, err := user.LookupGroup(group); err == nil {
		return strconv.Atoi(g.Gid)
	}
	// Fall back to a numeric gid.
	if n, err := strconv.Atoi(group); err == nil && n >= 0 {
		return n, nil
	}
	return 0, fmt.Errorf("unknown group (not a name or numeric gid)")
}

// parseMode parses an octal permission string like "0660" or "660".
func parseMode(mode string) (os.FileMode, error) {
	n, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("not octal")
	}
	if n > 0o777 {
		return 0, fmt.Errorf("mode %o out of range", n)
	}
	return os.FileMode(n), nil
}
