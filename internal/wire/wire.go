// Package wire defines the length-prefixed local-socket protocol spoken between
// the Slurm-side callers (the job_submit shim, the site_factor plugin, the
// completion feed) and obold. It is the contract from docs/SEAM_DESIGN.md §8.
//
// Three request kinds carry the job lifecycle:
//
//	GATE   — tier 1, hot: "escrow this cost?" The daemon mints a correlation
//	         token and escrows money against it in memory before replying.
//	BIND   — tier 2: bind a token to the Slurm job id once one is assigned;
//	         also the start-event / burst-reservation trigger.
//	SETTLE — tier 3: settle a job (complete/timeout/cancel/infrafail).
//
// plus PING for health/liveness checks. Framing mirrors the kernel WAL
// (internal/budget/wal.go): [u32 len][u32 crc32][payload], little-endian, with
// the payload a JSON-encoded Frame. Keeping one framing discipline in the repo
// means one set of torn-tail / corruption rules to reason about.
//
// The protocol is versioned (ProtocolVersion). Every Frame carries the version
// so a mismatched shim and daemon fail loudly rather than misparse.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// ProtocolVersion is the wire-format version. Bump it on any incompatible change
// to the Frame shape or the request/response types. The daemon rejects frames
// whose Version it does not understand.
const ProtocolVersion = 1

// MaxFrameSize bounds a single payload. Requests and responses are tiny; this is
// a guard against a corrupt length prefix causing a huge allocation, not a real
// limit on legitimate traffic.
const MaxFrameSize = 1 << 20 // 1 MiB

// Kind is the message discriminator carried in every Frame.
type Kind string

// Message kinds. Request kinds name a lifecycle event; each response echoes the
// request kind so a caller multiplexing on one connection can correlate.
const (
	KindGate   Kind = "gate"
	KindBind   Kind = "bind"
	KindSettle Kind = "settle"
	KindPing   Kind = "ping"
	KindStatus Kind = "status"
	KindTopUp  Kind = "topup"
	KindList   Kind = "list"
)

// SettleKind names how a job ended, routing to the matching kernel transition.
type SettleKind string

// Settle kinds map 1:1 onto the kernel's settlement transitions
// (see docs/SEAM_DESIGN.md §12).
const (
	SettleComplete  SettleKind = "complete"  // clean exit after Runtime seconds
	SettleTimeout   SettleKind = "timeout"   // hit walltime, no refund
	SettleCancel    SettleKind = "cancel"    // scancel; bill elapsed if started
	SettleInfraFail SettleKind = "infrafail" // NODE_FAIL / preempt; flag routes bill vs write-off
)

// TRES is the consumable-resource set read from job_desc at submit. It rides
// Slurm's existing accounting (docs/SEAM_DESIGN.md §5) rather than a parallel one.
// Fields are the GPU-aware set; zero values mean "not requested".
type TRES struct {
	CPUs int64 `json:"cpus,omitempty"`
	GPUs int64 `json:"gpus,omitempty"`
	Mem  int64 `json:"mem,omitempty"` // megabytes
}

// GateRequest is the hot-path submit gate (tier 1). The daemon resolves
// (Account, Partition) to a budget, computes cost from TimeLimit and the
// partition rate, and escrows in memory before replying. NTasks > 1 marks a job
// array (%N), routed to SubmitArray.
type GateRequest struct {
	Account   string `json:"account"`
	Partition string `json:"partition"`
	UID       uint32 `json:"uid"`
	TimeLimit int64  `json:"time_limit"` // requested walltime, seconds
	TRES      TRES   `json:"tres"`
	NTasks    int    `json:"ntasks"` // 1 = single job; >1 = array task count
}

// GateResponse is the gate verdict. On Allow the daemon has already escrowed
// against Token in memory (durability completes asynchronously under group
// commit). On reject, Reason carries a user-facing message for err_msg.
type GateResponse struct {
	Allow  bool   `json:"allow"`
	Token  string `json:"token,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// BindRequest binds a minted Token to the Slurm-assigned JobID once one exists
// (tier 2). After binding, the janitor can check liveness by job id. It also
// carries the start event / burst-reservation trigger.
type BindRequest struct {
	Token string `json:"token"`
	JobID string `json:"jobid"`
}

// BindResponse acknowledges a bind.
type BindResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// SettleRequest closes out a job (tier 3). Exactly one of Token or JobID
// identifies the escrow; JobID is preferred once bound. Runtime/Elapsed are
// seconds; Now is the daemon-supplied logical clock at the call.
type SettleRequest struct {
	Token   string     `json:"token,omitempty"`
	JobID   string     `json:"jobid,omitempty"`
	Kind    SettleKind `json:"kind"`
	Runtime int64      `json:"runtime,omitempty"` // for complete
	Elapsed int64      `json:"elapsed,omitempty"` // for cancel/infrafail
}

// SettleResponse acknowledges a settle.
type SettleResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// StatusRequest asks the daemon for a snapshot of a budget. Account selects
// which account's budget (multi-account, #18); empty means the sole account when
// only one is configured, else the daemon replies asking which.
type StatusRequest struct {
	Account string `json:"account,omitempty"`
}

// StatusResponse is a point-in-time snapshot for the `obol show` verb. It mirrors
// budget.Status; the daemon fills it from budget.Budget.Status(now).
type StatusResponse struct {
	C           int64 `json:"c"`
	B0          int64 `json:"b0"`
	B           int64 `json:"b"`
	Reserved    int64 `json:"reserved"`
	Consumed    int64 `json:"consumed"`
	WriteOff    int64 `json:"writeoff"`
	TS          int64 `json:"ts"`
	TE          int64 `json:"te"`
	LiveEscrows int   `json:"live_escrows"`
	LiveArrays  int   `json:"live_arrays"`

	BurstEnabled bool  `json:"burst_enabled"`
	BurstPot     int64 `json:"burst_pot,omitempty"`
	BurstCeiling int64 `json:"burst_ceiling,omitempty"`
	RLive        int64 `json:"rlive,omitempty"`

	Lapsed          bool  `json:"lapsed"`
	ConservationOK  bool  `json:"conservation_ok"`
	ConservationSum int64 `json:"conservation_sum"`
	TimeToEmpty     int64 `json:"time_to_empty"` // seconds; -1 if C<=0

	Account string `json:"account,omitempty"` // which account this snapshot is for
	OK      bool   `json:"ok"`                // false + Reason on a status error
	Reason  string `json:"reason,omitempty"`
}

// TopUpRequest adds money to an account's budget (admin-only). Amount is
// positive (add-only).
type TopUpRequest struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

// TopUpResponse acknowledges a top-up with the new balance.
type TopUpResponse struct {
	OK         bool   `json:"ok"`
	Reason     string `json:"reason,omitempty"`
	NewBalance int64  `json:"new_balance,omitempty"`
	NewB0      int64  `json:"new_b0,omitempty"`
}

// ListRequest asks for a summary of the accounts the caller may see.
type ListRequest struct{}

// ListAccount is one row of a ListResponse.
type ListAccount struct {
	Account  string `json:"account"`
	B        int64  `json:"b"`
	B0       int64  `json:"b0"`
	Reserved int64  `json:"reserved"`
	Consumed int64  `json:"consumed"`
	Live     int    `json:"live"`
	Lapsed   bool   `json:"lapsed"`
}

// ListResponse enumerates the visible accounts.
type ListResponse struct {
	OK       bool          `json:"ok"`
	Reason   string        `json:"reason,omitempty"`
	Accounts []ListAccount `json:"accounts,omitempty"`
}

// Frame is the on-wire envelope. Exactly one of the typed request/response
// payloads is populated, selected by MsgKind. Version guards against a
// shim/daemon mismatch.
type Frame struct {
	Version int  `json:"v"`
	MsgKind Kind `json:"k"`

	// Requests (one populated per request frame).
	Gate   *GateRequest   `json:"gate,omitempty"`
	Bind   *BindRequest   `json:"bind,omitempty"`
	Settle *SettleRequest `json:"settle,omitempty"`
	Status *StatusRequest `json:"status,omitempty"`
	TopUp  *TopUpRequest  `json:"topup,omitempty"`
	List   *ListRequest   `json:"list,omitempty"`

	// Responses (one populated per response frame).
	GateResp   *GateResponse   `json:"gate_resp,omitempty"`
	BindResp   *BindResponse   `json:"bind_resp,omitempty"`
	SettleResp *SettleResponse `json:"settle_resp,omitempty"`
	StatusResp *StatusResponse `json:"status_resp,omitempty"`
	TopUpResp  *TopUpResponse  `json:"topup_resp,omitempty"`
	ListResp   *ListResponse   `json:"list_resp,omitempty"`
}

// Sentinel errors surfaced by decode. ErrVersion is distinguished so a caller
// can report a version mismatch precisely rather than as generic corruption.
var (
	ErrVersion   = errors.New("wire: unsupported protocol version")
	ErrTooLarge  = errors.New("wire: frame exceeds max size")
	ErrCorrupt   = errors.New("wire: corrupt frame (crc mismatch)")
	ErrShortRead = errors.New("wire: short read")
)

var crcTable = crc32.MakeTable(crc32.IEEE)

// WriteFrame marshals f and writes one length-prefixed, crc-checked record to w.
// Framing: [u32 len][u32 crc32][payload]. It stamps the current ProtocolVersion.
func WriteFrame(w io.Writer, f *Frame) error {
	f.Version = ProtocolVersion
	payload, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameSize || len(payload) > math.MaxUint32 {
		return ErrTooLarge
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload))) //nolint:gosec // bounded above
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, crcTable))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads and validates one record from r, returning the decoded Frame.
// It enforces the size cap, the crc, and the protocol version. A clean io.EOF at
// a record boundary is returned verbatim so callers can detect a closed stream.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, ErrShortRead
		}
		return nil, err // io.EOF at a boundary is passed through
	}
	n := binary.LittleEndian.Uint32(hdr[0:4])
	want := binary.LittleEndian.Uint32(hdr[4:8])
	if n > MaxFrameSize {
		return nil, ErrTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, ErrShortRead
	}
	if crc32.Checksum(payload, crcTable) != want {
		return nil, ErrCorrupt
	}
	var f Frame
	if err := json.Unmarshal(payload, &f); err != nil {
		return nil, fmt.Errorf("wire: decode: %w", err)
	}
	if f.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrVersion, f.Version, ProtocolVersion)
	}
	return &f, nil
}

// --- convenience constructors: keep call sites from hand-building envelopes ---

// GateFrame wraps a GateRequest in a request Frame.
func GateFrame(req *GateRequest) *Frame { return &Frame{MsgKind: KindGate, Gate: req} }

// BindFrame wraps a BindRequest in a request Frame.
func BindFrame(req *BindRequest) *Frame { return &Frame{MsgKind: KindBind, Bind: req} }

// SettleFrame wraps a SettleRequest in a request Frame.
func SettleFrame(req *SettleRequest) *Frame { return &Frame{MsgKind: KindSettle, Settle: req} }

// PingFrame is a bare health-check request.
func PingFrame() *Frame { return &Frame{MsgKind: KindPing} }

// StatusFrame wraps a StatusRequest in a request Frame.
func StatusFrame(account string) *Frame {
	return &Frame{MsgKind: KindStatus, Status: &StatusRequest{Account: account}}
}

// TopUpFrame wraps a TopUpRequest in a request Frame.
func TopUpFrame(account string, amount int64) *Frame {
	return &Frame{MsgKind: KindTopUp, TopUp: &TopUpRequest{Account: account, Amount: amount}}
}

// ListFrame is a bare list request.
func ListFrame() *Frame { return &Frame{MsgKind: KindList, List: &ListRequest{}} }
