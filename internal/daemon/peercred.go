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
