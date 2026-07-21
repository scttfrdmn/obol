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
	reg     *Registry
	now     Clock
	weights Weights          // TRES->rate; zero-value = flat-rate (use each budget's C)
	ident   identityResolver // uid -> user/groups, only used for restricted accounts

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
	for {
		req, err := wire.ReadFrame(conn)
		if err != nil {
			return // EOF or protocol error: drop the connection
		}
		resp := s.dispatch(req)
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
func (s *Server) dispatch(req *wire.Frame) *wire.Frame {
	switch req.MsgKind {
	case wire.KindGate:
		return s.handleGate(req.Gate)
	case wire.KindBind:
		return s.handleBind(req.Bind)
	case wire.KindSettle:
		return s.handleSettle(req.Settle)
	case wire.KindStatus:
		return s.handleStatus(req.Status)
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
	c := s.weights.Rate(req.TRES) // 0 in flat-rate mode => kernel uses the budget's C
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
func (s *Server) handleStatus(req *wire.StatusRequest) *wire.Frame {
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
