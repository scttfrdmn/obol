//go:build !linux

package daemon

import "net"

// readPeerCred is the non-Linux fallback (dev machines, e.g. macOS). Peer-cred
// retrieval is platform-specific and not implemented here; it returns
// Available=false, which the authorizer treats as "cannot verify" — mutating
// verbs then fail closed when an admin list is configured. Production runs on
// Linux (peercred_linux.go), where creds are always available.
func readPeerCred(_ *net.UnixConn) PeerCred {
	return PeerCred{}
}
