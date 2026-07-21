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

// Server wraps a budget kernel and serves the wire protocol. It owns the
// token↔jobid binding table; the kernel keys escrows by the minted token, and
// BIND records the Slurm job id so a SETTLE carrying only the job id resolves
// back to its escrow.
type Server struct {
	bd      *budget.Budget
	now     Clock
	weights Weights // TRES->rate; zero-value = flat-rate (use bd.C)

	mu         sync.Mutex        // guards jobToToken
	jobToToken map[string]string // Slurm jobid -> escrow token
}

// New builds a Server over an existing budget with the given clock and flat-rate
// cost (no TRES weighting). The budget is expected to be durable
// (NewDurable/OpenBudget) in production, but any *budget.Budget works.
func New(bd *budget.Budget, now Clock) *Server {
	return NewWithWeights(bd, now, Weights{})
}

// NewWithWeights builds a Server that weights job cost by TRES (SEAM_DESIGN §5).
// Zero-value weights are flat-rate, identical to New.
func NewWithWeights(bd *budget.Budget, now Clock, w Weights) *Server {
	return &Server{
		bd:         bd,
		now:        now,
		weights:    w,
		jobToToken: make(map[string]string),
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
		return s.handleStatus()
	case wire.KindPing:
		return wire.PingFrame() // echo: liveness only
	default:
		return &wire.Frame{MsgKind: req.MsgKind, GateResp: &wire.GateResponse{
			Allow: false, Reason: fmt.Sprintf("unknown request kind %q", req.MsgKind),
		}}
	}
}

// handleGate is the hot path: mint a token, escrow the cost in the kernel, and
// reply. NTasks>1 routes to the array gate. The kernel computes cost = c·w
// internally from the budget's rate and the requested walltime.
func (s *Server) handleGate(req *wire.GateRequest) *wire.Frame {
	if req == nil {
		return gateReject("empty gate request")
	}
	token, err := mintToken()
	if err != nil {
		return gateReject("token mint failed")
	}
	now := s.now()
	c := s.weights.Rate(req.TRES) // 0 in flat-rate mode => kernel uses bd.C
	if req.NTasks > 1 {
		err = s.bd.SubmitArrayAt(token, c, req.NTasks, req.TimeLimit, now)
	} else {
		err = s.bd.SubmitAt(token, c, req.TimeLimit, now)
	}
	if err != nil {
		return gateReject(err.Error())
	}
	return &wire.Frame{MsgKind: wire.KindGate, GateResp: &wire.GateResponse{Allow: true, Token: token}}
}

// handleBind records the token↔jobid mapping and fires the start event
// (pending→running). For arrays the start event is per-task and arrives via a
// later mechanism; BIND here binds the array token so the janitor can track it.
func (s *Server) handleBind(req *wire.BindRequest) *wire.Frame {
	if req == nil || req.Token == "" || req.JobID == "" {
		return &wire.Frame{MsgKind: wire.KindBind, BindResp: &wire.BindResponse{OK: false, Reason: "token and jobid required"}}
	}
	s.mu.Lock()
	s.jobToToken[req.JobID] = req.Token
	s.mu.Unlock()

	// Start is best-effort: a 1:1 escrow transitions pending→running; an array
	// token has no 1:1 escrow, so ErrNoSuchJob here is expected and ignored.
	if err := s.bd.Start(req.Token, s.now()); err != nil && !errors.Is(err, budget.ErrNoSuchJob) {
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
	now := s.now()
	var err error
	switch req.Kind {
	case wire.SettleComplete:
		err = s.bd.Complete(token, req.Runtime, now)
	case wire.SettleTimeout:
		err = s.bd.Timeout(token, now)
	case wire.SettleCancel:
		err = s.bd.Cancel(token, req.Elapsed, now)
	case wire.SettleInfraFail:
		err = s.bd.InfraFail(token, req.Elapsed, now)
	default:
		return settleReject(fmt.Sprintf("unknown settle kind %q", req.Kind))
	}
	if err != nil {
		return settleReject(err.Error())
	}
	// Once settled, drop the jobid binding so the table doesn't grow unbounded.
	if req.JobID != "" {
		s.mu.Lock()
		delete(s.jobToToken, req.JobID)
		s.mu.Unlock()
	}
	return &wire.Frame{MsgKind: wire.KindSettle, SettleResp: &wire.SettleResponse{OK: true}}
}

// handleStatus returns a consistent snapshot of the budget for `obol show`. It
// reads the kernel's Status inspector (read-only, one lock) and maps it to the
// wire response.
func (s *Server) handleStatus() *wire.Frame {
	st := s.bd.Report(s.now())
	return &wire.Frame{MsgKind: wire.KindStatus, StatusResp: &wire.StatusResponse{
		C: st.C, B0: st.B0, B: st.B, Reserved: st.Reserved, Consumed: st.Consumed,
		WriteOff: st.WriteOff, TS: st.TS, TE: st.TE,
		LiveEscrows: st.LiveEscrows, LiveArrays: st.LiveArrays,
		BurstEnabled: st.BurstEnabled, BurstPot: st.BurstPot,
		BurstCeiling: st.BurstCeiling, RLive: st.RLive,
		Lapsed: st.Lapsed, ConservationOK: st.ConservationOK,
		ConservationSum: st.ConservationSum, TimeToEmpty: st.TimeToEmpty(),
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
