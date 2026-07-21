package daemon

import (
	"os/user"
	"strconv"
	"sync"
)

// Access enforcement (SEAM_DESIGN.md §9). By default obol TRUSTS Slurm: if a job
// reached the gate under --account=X, slurmdbd already authorized the submitter's
// membership, so obol charges without a second check. An account MAY set an
// allow-list (allow_users / allow_groups) to further restrict who may spend it;
// only then does obol resolve the submitter's uid to a user/group set and check
// it. This keeps the hot path free of uid→group lookups in the common case and
// avoids a mandatory parallel identity store.

// identityResolver maps a uid to a username and its group names. Injected so the
// hot path can be tested without real OS users.
type identityResolver interface {
	lookup(uid uint32) (username string, groups []string, err error)
}

// osIdentity resolves uids via the OS (nsswitch: local, sssd, LDAP, …). Results
// are cached because the lookup can be slow and is only hit for restricted
// accounts.
type osIdentity struct {
	mu    sync.Mutex
	cache map[uint32]identity
}

type identity struct {
	user   string
	groups []string
}

func (o *osIdentity) lookup(uid uint32) (string, []string, error) {
	o.mu.Lock()
	if o.cache == nil {
		o.cache = make(map[uint32]identity)
	}
	if id, ok := o.cache[uid]; ok {
		o.mu.Unlock()
		return id.user, id.groups, nil
	}
	o.mu.Unlock()

	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", nil, err
	}
	gids, err := u.GroupIds()
	if err != nil {
		return "", nil, err
	}
	groups := make([]string, 0, len(gids))
	for _, gid := range gids {
		if g, gerr := user.LookupGroupId(gid); gerr == nil {
			groups = append(groups, g.Name)
		}
	}
	o.mu.Lock()
	o.cache[uid] = identity{user: u.Username, groups: groups}
	o.mu.Unlock()
	return u.Username, groups, nil
}

// authorize reports whether uid may spend the given account's budget. Returns
// (true, "") when the account is unrestricted (the default — trust Slurm) or the
// uid is on its allow-list; (false, reason) otherwise. On a uid-lookup failure
// for a restricted account it fails closed (real money must not leak on an
// identity error).
func (s *Server) authorize(account string, uid uint32) (bool, string) {
	ac, ok := s.reg.access[account]
	if !ok || !ac.restricted() {
		return true, "" // unrestricted: Slurm already authorized account membership
	}
	username, groups, err := s.ident.lookup(uid)
	if err != nil {
		return false, "cannot resolve submitter identity for restricted account " + account
	}
	if contains(ac.AllowUsers, username) {
		return true, ""
	}
	for _, g := range groups {
		if contains(ac.AllowGroups, g) {
			return true, ""
		}
	}
	return false, "not authorized for account " + account
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// --- management-command authorization (peer-cred based) ---
//
// These gate the ADMIN CLI surface, distinct from the per-account job access
// above. They key on the connection's kernel-verified PeerCred (SO_PEERCRED),
// never the wire uid, so a client cannot claim to be an admin.

// requireAdmin authorizes a mutating management verb (topup; later create/move).
// Returns (true,"") when allowed, (false,reason) otherwise. root (uid 0) is
// always admin. When no admin list is configured, enforcement is OFF (the socket
// permissions are the boundary) and everything is allowed — preserving
// pre-authz behavior.
func (s *Server) requireAdmin(peer PeerCred) (bool, string) {
	if !s.reg.adminEnforced() {
		return true, "" // enforcement off: socket perms are the boundary
	}
	if !peer.Available {
		return false, "cannot verify caller identity (peer credentials unavailable)"
	}
	if peer.UID == 0 {
		return true, "" // root is always admin
	}
	username, groups, err := s.ident.lookup(peer.UID)
	if err != nil {
		return false, "cannot resolve caller identity"
	}
	if contains(s.reg.adminUsers, username) {
		return true, ""
	}
	for _, g := range groups {
		if contains(s.reg.adminGroups, g) {
			return true, ""
		}
	}
	return false, "not authorized: admin required"
}

// canRead reports whether the peer may read an account's status. Admins (and
// root) see everything. An account with no allow-list is public-readable (it
// matches its open-submit posture). Otherwise the peer must be on the account's
// allow-list (a member). When admin enforcement is off, reads are open.
func (s *Server) canRead(account string, peer PeerCred) bool {
	if !s.reg.adminEnforced() {
		return true
	}
	if ok, _ := s.requireAdmin(peer); ok {
		return true
	}
	ac, ok := s.reg.access[account]
	if !ok {
		return false
	}
	if !ac.restricted() {
		return true // open budget: public-readable
	}
	if !peer.Available {
		return false
	}
	username, groups, err := s.ident.lookup(peer.UID)
	if err != nil {
		return false
	}
	if contains(ac.AllowUsers, username) {
		return true
	}
	for _, g := range groups {
		if contains(ac.AllowGroups, g) {
			return true
		}
	}
	return false
}
