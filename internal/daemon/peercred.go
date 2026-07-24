package daemon

import "net"

// PeerCred is the kernel-verified identity of a socket peer — the real uid/gid of
// the connecting process, obtained from the OS, NOT the spoofable `uid` field a
// client puts on the wire. It is the basis for authorizing management commands.
type PeerCred struct {
	UID       uint32
	GID       uint32
	Available bool // false when the platform can't report peer creds (dev fallback)
}

// peerCredFunc reads the peer credentials of a Unix connection. It is a package
// variable so tests can inject a fake identity without a real socket peer; the
// production implementation (peerCredUnix, build-tagged per platform) reads the
// kernel-verified creds.
var peerCredFunc = peerCredOf

// peerCredOf returns the peer credentials for conn, or an Available=false zero
// value when conn is not a Unix connection or the platform can't report them.
func peerCredOf(conn net.Conn) PeerCred {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return PeerCred{}
	}
	return readPeerCred(uc) // platform-specific (peercred_linux.go / _other.go)
}

// isLocalConnFunc reports whether a connection is a local Unix-socket peer (as
// opposed to a TCP peer over the off-host transport, #144). A package var so
// tests can force the remote path without a real TCP listener. This is DISTINCT
// from PeerCred.Available: a Unix conn is local even on a platform that can't
// read SO_PEERCRED (e.g. macOS dev), so the TCP auth-token gate must key on the
// connection TYPE, not on cred availability.
var isLocalConnFunc = func(conn net.Conn) bool {
	_, ok := conn.(*net.UnixConn)
	return ok
}
