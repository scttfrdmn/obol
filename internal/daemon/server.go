// Package daemon is the obold server: it holds one budget kernel and serves the
// wire protocol (internal/wire) over a local stream socket. Each wire request is
// routed onto a proven kernel transition (docs/SEAM_DESIGN.md §12); the daemon
// adds no money or burst logic of its own.
//
// The daemon supplies `now` from its own clock at the moment of each call, which
// preserves the kernel invariant that transitions are pure functions of
// (state, command, now): the clock lives at the boundary, never inside a
// transition.
//
// MVP scope: one budget, resolved for every request. Multi-budget resolution by
// (account, partition) is deferred (issue #18); the token↔jobid binding and the
// single-budget lifecycle are what this server proves end to end.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// Clock returns the current logical time in seconds. The server calls it once
// per mutating request and passes the result into the kernel, keeping the kernel
// clock-free. Tests inject a deterministic clock.
type Clock func() budget.Seconds

// Server serves the wire protocol over a registry of per-account budgets
// (SEAM_DESIGN.md §9). GATE resolves the submission's account to its budget; BIND
// and SETTLE carry only a token/jobid, so the server records token→budget (and
// token→jobid) at gate time to route them back.
type Server struct {
	reg      *Registry
	now      Clock
	weights  Weights          // TRES->rate; zero-value = flat-rate (use each budget's C)
	nodeCost *NodeCost        // node-type rates (issue #65); nil/empty = not configured
	ident    identityResolver // uid -> user/groups, only used for restricted accounts

	mu          sync.Mutex                // guards jobToToken and tokenBudget
	jobToToken  map[string]string         // Slurm jobid -> escrow token
	tokenBudget map[string]*budget.Budget // escrow token -> owning account budget
}

// New builds a Server over a single budget (back-compat: wraps it in a
// one-account registry named "default"). Flat-rate cost, no TRES weighting.
func New(bd *budget.Budget, now Clock) *Server {
	return NewWithWeights(bd, now, Weights{})
}

// NewWithWeights builds a single-budget Server that weights cost by TRES.
func NewWithWeights(bd *budget.Budget, now Clock, w Weights) *Server {
	reg := &Registry{
		budgets: map[string]*budget.Budget{"default": bd},
		access:  map[string]AccountConfig{"default": {Name: "default"}},
	}
	return NewWithRegistry(reg, now, w)
}

// NewWithRegistry builds a Server over a multi-account registry. This is the
// multi-budget path used by obold -config.
func NewWithRegistry(reg *Registry, now Clock, w Weights) *Server {
	return &Server{
		reg:         reg,
		now:         now,
		weights:     w,
		ident:       &osIdentity{}, // real uid->group lookup; swapped in tests
		jobToToken:  make(map[string]string),
		tokenBudget: make(map[string]*budget.Budget),
	}
}

// SetNodeCost enables node-type pricing (issue #65). When set, the gate escrows
// a partition's worst-case node rate and BIND reprices to the actual node.
func (s *Server) SetNodeCost(nc *NodeCost) { s.nodeCost = nc }

// Serve accepts connections on ln until it is closed, handling each in its own
// goroutine. It returns when the listener is closed (a clean shutdown), swallowing
// the resulting accept error.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

// handleConn serves frames on one connection until EOF or a read error. The shim
// makes one request per connection on the hot path, but the loop supports a
// long-lived multiplexed connection too (e.g. the CLI).
func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	// Read the kernel-verified peer identity once per connection; management verbs
	// authorize against it (not the spoofable wire uid).
	peer := peerCredFunc(conn)
	for {
		req, err := wire.ReadFrame(conn)
		if err != nil {
			return // EOF or protocol error: drop the connection
		}
		resp := s.dispatch(req, peer)
		if resp == nil {
			continue
		}
		if err := wire.WriteFrame(conn, resp); err != nil {
			return
		}
	}
}

// dispatch routes one request frame to its handler and returns the response
// frame. An unknown or malformed kind yields a rejecting response rather than a
// dropped connection, so a version-skewed shim gets a clear answer.
func (s *Server) dispatch(req *wire.Frame, peer PeerCred) *wire.Frame {
	switch req.MsgKind {
	case wire.KindGate:
		return s.handleGate(req.Gate)
	case wire.KindBind:
		return s.handleBind(req.Bind)
	case wire.KindSettle:
		return s.handleSettle(req.Settle)
	case wire.KindStatus:
		return s.handleStatus(req.Status, peer)
	case wire.KindTopUp:
		return s.handleTopUp(req.TopUp, peer)
	case wire.KindList:
		return s.handleList(peer)
	case wire.KindLog:
		return s.handleLog(req.Log, peer)
	case wire.KindSetRate:
		return s.handleSetRate(req.SetRate, peer)
	case wire.KindSetWindow:
		return s.handleSetWindow(req.SetWindow, peer)
	case wire.KindResolve:
		return s.handleResolve(req.Resolve, peer)
	case wire.KindSimulate:
		return s.handleSimulate(req.Simulate, peer)
	case wire.KindCreate:
		return s.handleCreate(req.Create, peer)
	case wire.KindAttach:
		return s.handleAttach(req.Attach, peer)
	case wire.KindPing:
		return wire.PingFrame() // echo: liveness only
	default:
		return &wire.Frame{MsgKind: req.MsgKind, GateResp: &wire.GateResponse{
			Allow: false, Reason: fmt.Sprintf("unknown request kind %q", req.MsgKind),
		}}
	}
}

// handleGate is the hot path: resolve the account to its budget, (optionally)
// authorize the submitter, mint a token, and escrow the cost. NTasks>1 routes to
// the array gate. The kernel computes cost = c·w from the budget's rate and the
// requested walltime.
func (s *Server) handleGate(req *wire.GateRequest) *wire.Frame {
	if req == nil {
		return gateReject("empty gate request")
	}
	// Resolve account -> budget (SEAM §9: none resolves -> reject).
	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		return gateReject(err.Error())
	}
	// Optional access check (no-op unless the account has an allow-list). Done
	// before any escrow so an unauthorized submit costs nothing.
	if ok, reason := s.authorize(req.Account, req.UID); !ok {
		return gateReject(reason)
	}
	token, err := mintToken()
	if err != nil {
		return gateReject("token mint failed")
	}
	now := s.now()
	// Cost rate: node-type worst-case (if this partition has node types
	// configured) takes precedence — the gate can't know the node yet, so it
	// escrows the most expensive it could land on; BIND reprices down to the
	// actual node. Otherwise fall back to TRES weighting / the budget's flat rate.
	c := s.nodeCost.worstRate(req.Partition)
	if c == 0 {
		c = s.weights.Rate(req.TRES) // 0 => kernel uses the budget's C
	}
	if req.NTasks > 1 {
		err = bd.SubmitArrayAt(token, c, req.NTasks, req.TimeLimit, now)
	} else {
		err = bd.SubmitAt(token, c, req.TimeLimit, now)
	}
	if err != nil {
		return gateReject(err.Error())
	}
	// Record token -> owning budget so BIND/SETTLE (which carry no account) route
	// back to the right account's budget.
	s.mu.Lock()
	s.tokenBudget[token] = bd
	s.mu.Unlock()
	return &wire.Frame{MsgKind: wire.KindGate, GateResp: &wire.GateResponse{Allow: true, Token: token}}
}

// budgetForToken returns the budget that owns an escrow token, recorded at gate.
func (s *Server) budgetForToken(token string) (*budget.Budget, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bd, ok := s.tokenBudget[token]
	return bd, ok
}

// handleBind records the token↔jobid mapping and fires the start event
// (pending→running). For arrays the start event is per-task and arrives via a
// later mechanism; BIND here binds the array token so the janitor can track it.
func (s *Server) handleBind(req *wire.BindRequest) *wire.Frame {
	if req == nil || req.Token == "" || req.JobID == "" {
		return &wire.Frame{MsgKind: wire.KindBind, BindResp: &wire.BindResponse{OK: false, Reason: "token and jobid required"}}
	}
	bd, ok := s.budgetForToken(req.Token)
	if !ok {
		return &wire.Frame{MsgKind: wire.KindBind, BindResp: &wire.BindResponse{OK: false, Reason: "unknown token"}}
	}
	s.mu.Lock()
	s.jobToToken[req.JobID] = req.Token
	s.mu.Unlock()

	// Node-type cost true-up (issue #65): the gate escrowed the partition's
	// worst-case node rate; now Slurm has bound a real node, reprice to its rate
	// BEFORE Start (the rate commits into rLive/burst at Start). Only applies when
	// node-type pricing is configured and the bound node type has a known rate;
	// Reprice only ever lowers (worst >= actual). A no-op / unknown node type
	// keeps the worst-case escrow. Best-effort: a reprice failure (e.g. the node
	// rate isn't actually lower — misconfig) is logged-by-return but does not fail
	// the bind, since the worst-case escrow is already safe.
	if req.NodeType != "" && s.nodeCost.enabled() {
		if rate := s.nodeCost.rate(req.NodeType); rate > 0 {
			_ = bd.Reprice(req.Token, rate, s.now()) // safe to ignore: worst-case stands on failure
		}
	}

	// Start is best-effort: a 1:1 escrow transitions pending→running; an array
	// token has no 1:1 escrow, so ErrNoSuchJob here is expected and ignored.
	if err := bd.Start(req.Token, s.now()); err != nil && !errors.Is(err, budget.ErrNoSuchJob) {
		return &wire.Frame{MsgKind: wire.KindBind, BindResp: &wire.BindResponse{OK: false, Reason: err.Error()}}
	}
	return &wire.Frame{MsgKind: wire.KindBind, BindResp: &wire.BindResponse{OK: true}}
}

// handleSettle resolves the escrow (by token, or by jobid via the bind table)
// and applies the matching kernel settlement transition.
func (s *Server) handleSettle(req *wire.SettleRequest) *wire.Frame {
	if req == nil {
		return settleReject("empty settle request")
	}
	token := req.Token
	if token == "" && req.JobID != "" {
		s.mu.Lock()
		token = s.jobToToken[req.JobID]
		s.mu.Unlock()
	}
	if token == "" {
		return settleReject("no token: unknown job")
	}
	bd, ok := s.budgetForToken(token)
	if !ok {
		return settleReject("no such escrow") // unknown/already-settled token
	}
	now := s.now()
	var err error
	switch req.Kind {
	case wire.SettleComplete:
		err = bd.Complete(token, req.Runtime, now)
	case wire.SettleTimeout:
		err = bd.Timeout(token, now)
	case wire.SettleCancel:
		err = bd.Cancel(token, req.Elapsed, now)
	case wire.SettleInfraFail:
		err = bd.InfraFail(token, req.Elapsed, now)
	default:
		return settleReject(fmt.Sprintf("unknown settle kind %q", req.Kind))
	}
	if err != nil {
		return settleReject(err.Error())
	}
	// Once settled, drop the jobid binding and token→budget entry so the tables
	// don't grow unbounded.
	s.mu.Lock()
	if req.JobID != "" {
		delete(s.jobToToken, req.JobID)
	}
	delete(s.tokenBudget, token)
	s.mu.Unlock()
	return &wire.Frame{MsgKind: wire.KindSettle, SettleResp: &wire.SettleResponse{OK: true}}
}

// handleStatus returns a consistent snapshot of an account's budget for
// `obol show`. When the request names an account, that budget is used; when it
// doesn't, the sole account is used if exactly one is configured, else an error
// asks which. A status error is carried as a rejecting StatusResp (Reason).
func (s *Server) handleStatus(req *wire.StatusRequest, peer PeerCred) *wire.Frame {
	var bd *budget.Budget
	account := ""
	if req != nil {
		account = req.Account
	}
	if account != "" {
		var err error
		if bd, err = s.reg.Resolve(account); err != nil {
			return statusReject(err.Error())
		}
	} else if name, only, ok := s.reg.Single(); ok {
		bd, account = only, name
	} else {
		return statusReject("multiple accounts configured; specify --account")
	}
	// Read visibility: admins see all; non-admins see their accounts + open ones.
	if !s.canRead(account, peer) {
		return statusReject("not authorized to view account " + account)
	}
	st := bd.Report(s.now())
	return &wire.Frame{MsgKind: wire.KindStatus, StatusResp: &wire.StatusResponse{
		C: st.C, B0: st.B0, B: st.B, Reserved: st.Reserved, Consumed: st.Consumed,
		WriteOff: st.WriteOff, TS: st.TS, TE: st.TE,
		LiveEscrows: st.LiveEscrows, LiveArrays: st.LiveArrays,
		BurstEnabled: st.BurstEnabled, BurstPot: st.BurstPot,
		BurstCeiling: st.BurstCeiling, RLive: st.RLive,
		Lapsed: st.Lapsed, ConservationOK: st.ConservationOK,
		ConservationSum: st.ConservationSum, TimeToEmpty: st.TimeToEmpty(),
		Account: account, OK: true,
	}}
}

// handleTopUp adds money to an account's budget. It is a MUTATING verb: the
// caller must be an admin (kernel-verified peer uid), checked before any change.
func (s *Server) handleTopUp(req *wire.TopUpRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return topUpReject("empty topup request")
	}
	if ok, reason := s.requireAdmin(peer); !ok {
		return topUpReject(reason)
	}
	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		return topUpReject(err.Error())
	}
	if err := bd.TopUp(req.Amount, s.now()); err != nil {
		return topUpReject(err.Error())
	}
	st := bd.Report(s.now())
	return &wire.Frame{MsgKind: wire.KindTopUp, TopUpResp: &wire.TopUpResponse{
		OK: true, NewBalance: st.B, NewB0: st.B0,
	}}
}

// handleList enumerates the accounts the caller may see (admins: all; others:
// their accounts + open budgets).
func (s *Server) handleList(peer PeerCred) *wire.Frame {
	now := s.now()
	var rows []wire.ListAccount
	for _, name := range s.reg.Names() {
		if !s.canRead(name, peer) {
			continue
		}
		bd, err := s.reg.Resolve(name)
		if err != nil {
			continue
		}
		st := bd.Report(now)
		rows = append(rows, wire.ListAccount{
			Account: name, B: st.B, B0: st.B0, Reserved: st.Reserved,
			Consumed: st.Consumed, Live: st.LiveEscrows + st.LiveArrays, Lapsed: st.Lapsed,
		})
	}
	return &wire.Frame{MsgKind: wire.KindList, ListResp: &wire.ListResponse{OK: true, Accounts: rows}}
}

// handleLog renders an account's WAL as a time-ordered audit log. Read verb:
// visibility-scoped like show/list. It reads the WAL file directly (read-only,
// append-only file) rather than the live budget, so it never contends the gate.
func (s *Server) handleLog(req *wire.LogRequest, peer PeerCred) *wire.Frame {
	account := ""
	if req != nil {
		account = req.Account
	}
	if account == "" {
		if name, _, ok := s.reg.Single(); ok {
			account = name
		} else {
			return logReject("multiple accounts configured; specify --account")
		}
	}
	bd, err := s.reg.Resolve(account)
	if err != nil {
		return logReject(err.Error())
	}
	if !s.canRead(account, peer) {
		return logReject("not authorized to view account " + account)
	}
	entries, err := bd.Log()
	if err != nil {
		return logReject(err.Error())
	}
	rows := make([]wire.LogEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, wire.LogEntry{
			Kind: e.Kind, JobID: e.JobID, ArrayID: e.ArrayID, Idx: e.Idx, N: e.N,
			Rate: e.Rate, W: e.W, Runtime: e.Runtime, Elapsed: e.Elapsed,
			Amount: e.Amount, TS: e.TS, TE: e.TE, Xfer: e.Xfer, Now: e.Now,
		})
	}
	return &wire.Frame{MsgKind: wire.KindLog, LogResp: &wire.LogResponse{
		OK: true, Account: account, Entries: rows,
	}}
}

// handleSetRate changes an account's flat cost rate. Mutating verb → admin only.
func (s *Server) handleSetRate(req *wire.SetRateRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return ackReject("empty set-rate request")
	}
	if ok, reason := s.requireAdmin(peer); !ok {
		return ackReject(reason)
	}
	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		return ackReject(err.Error())
	}
	if err := bd.SetRate(req.Rate, s.now()); err != nil {
		return ackReject(err.Error())
	}
	return &wire.Frame{MsgKind: wire.KindSetRate, AckResp: &wire.AckResponse{OK: true}}
}

// handleSetWindow changes an account's time window. Mutating verb → admin only.
func (s *Server) handleSetWindow(req *wire.SetWindowRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return ackReject("empty set-window request")
	}
	if ok, reason := s.requireAdmin(peer); !ok {
		return ackReject(reason)
	}
	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		return ackReject(err.Error())
	}
	if err := bd.SetWindow(req.TS, req.TE, s.now()); err != nil {
		return ackReject(err.Error())
	}
	return &wire.Frame{MsgKind: wire.KindSetWindow, AckResp: &wire.AckResponse{OK: true}}
}

// handleResolve is a DRY RUN of the gate's decision for a submission: it reports
// which budget the account resolves to, the effective rate and its source, the
// access verdict, and whether the gate would admit — escrowing nothing. Read
// verb: visibility-scoped like show/list.
func (s *Server) handleResolve(req *wire.ResolveRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return resolveReject("empty resolve request")
	}
	resp := &wire.ResolveResponse{OK: true, Account: req.Account}

	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		// Not resolved: report the (non-)decision rather than erroring.
		resp.Resolved = false
		resp.Admits = false
		resp.Decision = "no budget resolves for account " + req.Account + " → reject (no funded path)"
		return &wire.Frame{MsgKind: wire.KindResolve, ResolveResp: resp}
	}
	resp.Resolved = true

	// Read scoping: a caller may only diagnose accounts they can see.
	if !s.canRead(req.Account, peer) {
		return resolveReject("not authorized to view account " + req.Account)
	}

	st := bd.Report(s.now())
	resp.Balance = st.B
	resp.Rate, resp.RateSource = s.effectiveRate(req.Partition, req.TRES, bd, s.now())

	// Access verdict (does not escrow).
	authorized, areason := s.authorize(req.Account, req.UID)
	resp.Authorized = authorized

	// Cost + funding/window checks if a walltime was given.
	inWindow := s.now() >= st.TS && s.now() < st.TE && !st.Lapsed
	funded := true
	if req.TimeLimit > 0 {
		resp.Cost = resp.Rate * req.TimeLimit
		funded = resp.Cost <= st.B
	}

	switch {
	case !authorized:
		resp.Admits, resp.Decision = false, areason
	case !inWindow:
		resp.Admits, resp.Decision = false, "budget window closed/lapsed → reject"
	case !funded:
		resp.Admits, resp.Decision = false, fmt.Sprintf("insufficient budget: cost %d > balance %d", resp.Cost, st.B)
	default:
		resp.Admits = true
		if req.TimeLimit > 0 {
			resp.Decision = fmt.Sprintf("admit: account %s, rate %d (%s), cost %d ≤ balance %d",
				req.Account, resp.Rate, resp.RateSource, resp.Cost, st.B)
		} else {
			resp.Decision = fmt.Sprintf("admit: resolves to %s, rate %d (%s); pass --time-limit to check funding",
				req.Account, resp.Rate, resp.RateSource)
		}
	}
	return &wire.Frame{MsgKind: wire.KindResolve, ResolveResp: resp}
}

// effectiveRate returns the per-second rate the gate would use for a submission
// and a human label for its source, mirroring handleGate's precedence: node-type
// worst-case (if the partition has node types), else TRES weighting, else the
// budget's flat C.
func (s *Server) effectiveRate(partition string, tres wire.TRES, bd *budget.Budget, now budget.Seconds) (budget.Units, string) {
	if r := s.nodeCost.worstRate(partition); r > 0 {
		return r, "node-type worst-case"
	}
	if r := s.weights.Rate(tres); r > 0 {
		return r, "tres"
	}
	return bd.Report(now).C, "flat"
}

// handleSimulate dry-runs a hypothetical submission: cost, funding, runway, and
// the gate verdict — committing nothing. Read verb: visibility-scoped.
func (s *Server) handleSimulate(req *wire.SimulateRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return simulateReject("empty simulate request")
	}
	bd, err := s.reg.Resolve(req.Account)
	if err != nil {
		return simulateReject(err.Error())
	}
	if !s.canRead(req.Account, peer) {
		return simulateReject("not authorized to view account " + req.Account)
	}
	now := s.now()
	rate, source := s.effectiveRate(req.Partition, req.TRES, bd, now)
	sim := bd.Simulate(rate, req.TimeLimit, now)
	return &wire.Frame{MsgKind: wire.KindSimulate, SimulateResp: &wire.SimulateResponse{
		OK: true, Account: req.Account, Rate: rate, RateSource: source,
		Cost: sim.Cost, Balance: bd.Report(now).B, Admit: sim.Admit,
		Deny: sim.Reason, Runway: sim.Runway,
	}}
}

// handleCreate creates a new account budget at runtime. Mutating verb → admin.
func (s *Server) handleCreate(req *wire.CreateRequest, peer PeerCred) *wire.Frame {
	if req == nil {
		return ackReject("empty create request")
	}
	if ok, reason := s.requireAdmin(peer); !ok {
		return ackReject(reason)
	}
	err := s.reg.Create(AccountConfig{
		Name: req.Account, Balance: req.Balance, Rate: req.Rate, Window: req.Window,
		AllowUsers: req.AllowUsers, AllowGroups: req.AllowGroups,
	})
	if err != nil {
		return ackReject(err.Error())
	}
	return &wire.Frame{MsgKind: wire.KindCreate, AckResp: &wire.AckResponse{OK: true}}
}

// handleAttach adds or removes users/groups on an account's access list.
// Mutating verb → admin.
func (s *Server) handleAttach(req *wire.AttachRequest, peer PeerCred) *wire.Frame {
	attachReject := func(reason string) *wire.Frame {
		return &wire.Frame{MsgKind: wire.KindAttach, AttachResp: &wire.AttachResponse{OK: false, Reason: reason}}
	}
	if req == nil {
		return attachReject("empty attach request")
	}
	if ok, reason := s.requireAdmin(peer); !ok {
		return attachReject(reason)
	}
	ac, ok := s.reg.accessOf(req.Account)
	if !ok {
		return attachReject((&budgetErr{req.Account}).Error())
	}
	users, groups := ac.AllowUsers, ac.AllowGroups
	if req.Detach {
		users = removeAll(users, req.Users)
		groups = removeAll(groups, req.Groups)
	} else {
		users = addAll(users, req.Users)
		groups = addAll(groups, req.Groups)
	}
	if err := s.reg.SetAccess(req.Account, users, groups); err != nil {
		return attachReject(err.Error())
	}
	return &wire.Frame{MsgKind: wire.KindAttach, AttachResp: &wire.AttachResponse{
		OK: true, AllowUsers: users, AllowGroups: groups,
	}}
}

// budgetErr formats a consistent "no budget for account" message.
type budgetErr struct{ account string }

func (e *budgetErr) Error() string { return "no budget for account: " + e.account }

// addAll returns set ∪ add (order-preserving, deduped).
func addAll(set, add []string) []string {
	out := append([]string(nil), set...)
	for _, v := range add {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// removeAll returns set minus rm.
func removeAll(set, rm []string) []string {
	out := set[:0:0]
	for _, v := range set {
		if !contains(rm, v) {
			out = append(out, v)
		}
	}
	return out
}

// mintToken returns a unique, unforgeable correlation token. The "budget:" prefix
// matches what the shim stamps into admin_comment (docs/SEAM_DESIGN.md §4).
func mintToken() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return "budget:" + hex.EncodeToString(b[:]), nil
}

func gateReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindGate, GateResp: &wire.GateResponse{Allow: false, Reason: reason}}
}

func settleReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindSettle, SettleResp: &wire.SettleResponse{OK: false, Reason: reason}}
}

func statusReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindStatus, StatusResp: &wire.StatusResponse{OK: false, Reason: reason}}
}

func topUpReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindTopUp, TopUpResp: &wire.TopUpResponse{OK: false, Reason: reason}}
}

func logReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindLog, LogResp: &wire.LogResponse{OK: false, Reason: reason}}
}

func ackReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindSetRate, AckResp: &wire.AckResponse{OK: false, Reason: reason}}
}

func resolveReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindResolve, ResolveResp: &wire.ResolveResponse{OK: false, Reason: reason}}
}

func simulateReject(reason string) *wire.Frame {
	return &wire.Frame{MsgKind: wire.KindSimulate, SimulateResp: &wire.SimulateResponse{OK: false, Reason: reason}}
}
