//go:build linux

package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// readPeerCred reads SO_PEERCRED from the Unix socket — the kernel's record of
// the connecting process's real uid/gid. This is the production path (the daemon
// runs on the Linux head node) and cannot be spoofed by the client.
func readPeerCred(uc *net.UnixConn) PeerCred {
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCred{}
	}
	var cred *unix.Ucred
	var credErr error
	if cerr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return PeerCred{}
	}
	if credErr != nil || cred == nil {
		return PeerCred{}
	}
	return PeerCred{UID: cred.Uid, GID: cred.Gid, Available: true}
}
