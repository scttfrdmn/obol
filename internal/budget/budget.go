package budget

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Money is integer units. c (cost rate) is units/second, w (walltime) seconds.
// cost = c*w is therefore an exact integer. Conservation is checked with ==,
// so floating point is banned by construction.
// ---------------------------------------------------------------------------

// Units is an integer money amount. All money is integer so conservation is
// checked with == and floats are banned by construction.
type Units = int64

// Seconds is an integer count of logical-clock seconds (walltime, runtime, now).
type Seconds = int64

// Sentinel errors returned by the gate and settle transitions. Compare with
// errors.Is; they are wrapped where they cross a boundary.
var (
	ErrInsufficient = errors.New("insufficient budget")
	ErrRateExceeded = errors.New("burst rate exceeded")
	ErrLapsed       = errors.New("budget lapsed")
	ErrNotActive    = errors.New("budget not active")
	ErrNoSuchJob    = errors.New("no such escrow")
	ErrBadState     = errors.New("illegal transition")
	ErrBurstDrawCap = errors.New("burst draw cap exceeded")
	ErrBurstInsuff  = errors.New("insufficient burst banked")
)

// Status is the budget's lifecycle state for the time window.
type Status int

// Budget statuses. Active accepts submits; Lapsed is closed to new submits
// while live escrows settle normally.
const (
	Active Status = iota
	Lapsed
)

// Escrow is the per-submission reservation. In the 1:1 model one escrow == one
// job. reserved is what was debited at submit; w is the funded walltime.
type Escrow struct {
	JobID     string
	Reserved  Units
	W         Seconds
	Started   bool
	BurstResv Units // tokens reserved at dispatch; refunded tail at settle
}

// Budget is the single pot. All mutation goes through methods that take mu.
type Budget struct {
	mu sync.Mutex

	C  Units   // cost per second
	TS Seconds // period start (logical clock units)
	TE Seconds // period end

	B             Units // current balance
	ReservedTotal Units // denormalized Σ reserved(live) — keeps rate check O(1)
	Consumed      Units // Σ c*actual_runtime, billed to the user
	WriteOff      Units // Σ c*runtime absorbed by the system (infra failures, flag off)

	B0 Units // original allocation, for the conservation check

	// Policy flags (per budget). These, along with C/TS/TE above, are CONFIG:
	// set at creation, captured in the initial snapshot, and immutable thereafter
	// (issue #8). They are not logged commands, so WAL replay never changes them.
	// If mutation is ever added (e.g. an `obol set-rate` verb), it MUST be a
	// logged command applied through the replay path — a snapshot-only change
	// would lose its ordering against the command stream and break the
	// pure-(state, command, now) replay invariant.
	BillInfraFailures bool    // on = cloud (user pays infra loss); off = on-prem (write off)
	AllowRequeue      bool    // on = on-prem; off = cloud (requeue -> cancel)
	K                 float64 // burst ceiling multiplier; <=0 means infinite (disabled)

	// Burst token bucket (opt-in; zero-value disabled).
	BurstEnabled    bool
	BurstCeilingPct float64 // ceiling = pct * B0
	BurstDrawCap    Units   // max tokens one job may reserve; 0 = unlimited
	burstPot        Units
	fracAcc         Units   // sub-unit accrual remainder, numerator over window T
	rLive           Units   // aggregate live burn rate (Σ C over live jobs)
	lastTouch       Seconds // for lazy accrual

	escrows map[string]*Escrow
	arrays  map[string]*ArrayEscrow
	status  Status

	// Durability (nil/empty when not enabled; all existing behavior unchanged).
	wal       *WAL
	dir       string
	replaying bool

	// Tier-2 lock-cheap read path (see readview.go): every mutation publishes an
	// immutable snapshot of (B, burstPot, rLive) here under bd.mu; ReadSnapshot
	// loads it lock-free, so priority/burst reads never contend the gate lock.
	readView atomic.Pointer[ReadView]
}

// New constructs an in-memory (non-durable) budget with cost rate c, initial
// allocation b0, and time window [ts, te). Burst is disabled by default.
func New(c, b0, ts, te Units) *Budget {
	bd := &Budget{
		C: c, B: b0, B0: b0, TS: ts, TE: te,
		escrows:   make(map[string]*Escrow),
		lastTouch: ts, // accrual measured from window start
		status:    Active,
		K:         0, // infinite by default: dumping allowed
	}
	bd.publishLocked() // seed the tier-2 read view before any reader can load it
	return bd
}

// rateOK evaluates the optional burst ceiling. now is a logical clock.
// k<=0 disables it. Uses ReservedTotal so it's O(1), no scan.
func (bd *Budget) rateOK(cost Units, now Seconds) bool {
	if bd.K <= 0 {
		return true
	}
	rem := bd.TE - now
	if rem <= 0 {
		// At/after period end r->inf; submit is closed anyway via Lapse,
		// but guard against div-by-zero defensively.
		return false
	}
	r := float64(bd.B) / float64(rem)               // sustainable rate now
	return float64(bd.ReservedTotal+cost) <= bd.K*r // aggregate, not per-job
}

// Submit is THE GATE. Atomic: both inequalities and both counter mutations
// happen under one lock. Returns the live escrow on success.
func (bd *Budget) Submit(jobID string, w Seconds, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	defer bd.publishLocked() // refresh tier-2 read view before releasing the lock

	if bd.status != Active {
		return ErrLapsed
	}
	if now < bd.TS || now >= bd.TE {
		return ErrLapsed
	}
	if _, dup := bd.escrows[jobID]; dup {
		return ErrBadState
	}
	cost := bd.C * w
	if cost > bd.B { // solvency
		return ErrInsufficient
	}
	if !bd.rateOK(cost, now) { // optional legacy burst ceiling
		return ErrRateExceeded
	}
	// Money escrow only. Burst is reserved at dispatch (Start), not submit,
	// because burst is a concurrency property and concurrency isn't known yet.
	if err := bd.logCmd(Command{Kind: KindSubmit, JobID: jobID, W: w, Now: now}); err != nil {
		return err
	}
	bd.B -= cost
	bd.ReservedTotal += cost
	bd.escrows[jobID] = &Escrow{JobID: jobID, Reserved: cost, W: w}
	return nil
}

// Start marks an escrowed job as dispatched (pending->running). When burst is
// enabled it runs the burst dispatch gate, reserving excess tokens from the
// bank; a job that cannot reserve is rejected so the caller can retry later.
func (bd *Budget) Start(jobID string, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	defer bd.publishLocked()
	e, ok := bd.escrows[jobID]
	if !ok {
		return ErrNoSuchJob
	}
	if e.Started {
		return nil
	}
	// Burst dispatch gate: a job pushing aggregate burn above r0 must reserve
	// excess*w tokens from the bank. If it can't, the job waits (caller retries).
	if bd.BurstEnabled {
		bd.accrue(now)
		resv := bd.burstReserveFor(e.W)
		if resv > 0 {
			if bd.BurstDrawCap > 0 && resv > bd.BurstDrawCap {
				return ErrBurstDrawCap
			}
			if resv > bd.burstPot {
				return ErrBurstInsuff
			}
			if err := bd.logCmd(Command{Kind: KindStart, JobID: jobID, Now: now}); err != nil {
				return err
			}
			bd.burstPot -= resv
			e.BurstResv = resv
			bd.rLive += bd.C
			e.Started = true
			return nil
		}
		bd.rLive += bd.C
	}
	if err := bd.logCmd(Command{Kind: KindStart, JobID: jobID, Now: now}); err != nil {
		return err
	}
	e.Started = true
	return nil
}

// settle is the shared refund core. consumedRuntime is billed (or written off);
// the unused tail is refunded to B. Removes the escrow.
func (bd *Budget) settle(jobID string, runtime Seconds, writeOff bool, now Seconds) error {
	e, ok := bd.escrows[jobID]
	if !ok {
		return ErrNoSuchJob
	}
	if bd.BurstEnabled {
		bd.accrue(now) // fast-forward with this job still counted in rLive
	}
	if runtime < 0 {
		runtime = 0
	}
	if runtime > e.W {
		runtime = e.W
	}
	used := bd.C * runtime
	refund := e.Reserved - used // = c*(w-runtime), >=0

	bd.ReservedTotal -= e.Reserved
	bd.B += refund
	if writeOff {
		bd.WriteOff += used
	} else {
		bd.Consumed += used
	}

	// Burst: if this job was dispatched (Started), it was burning at C over the
	// interval just ended. Accrue with it still counted, then drop it from rLive
	// and refund the unused token tail. A job that never started reserved no burst.
	if bd.BurstEnabled && e.Started {
		bd.rLive -= bd.C
		if bd.rLive < 0 {
			bd.rLive = 0
		}
		if e.BurstResv > 0 && e.W > 0 {
			refundTok := e.BurstResv * (e.W - runtime) / e.W // proportional unused tail
			bd.burstPot += refundTok
			if c := bd.burstCeiling(); bd.burstPot > c {
				bd.burstPot = c
			}
		}
	}

	delete(bd.escrows, jobID)
	bd.publishLocked() // shared settle core: refreshes the tier-2 view for all settle variants
	return nil
}

// Complete settles a clean exit after `runtime` seconds (runtime<=w). The used
// portion is billed to the user and the unused tail refunded.
func (bd *Budget) Complete(jobID string, runtime, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if _, ok := bd.escrows[jobID]; !ok {
		return ErrNoSuchJob
	}
	if err := bd.logCmd(Command{Kind: KindComplete, JobID: jobID, Runtime: runtime, Now: now}); err != nil {
		return err
	}
	return bd.settle(jobID, runtime, false, now)
}

// Timeout settles a job that hit its walltime: runtime==w, refund 0.
// (Convenience over Complete.)
func (bd *Budget) Timeout(jobID string, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	e, ok := bd.escrows[jobID]
	if !ok {
		return ErrNoSuchJob
	}
	if err := bd.logCmd(Command{Kind: KindTimeout, JobID: jobID, Now: now}); err != nil {
		return err
	}
	return bd.settle(jobID, e.W, false, now)
}

// Cancel settles a cancelled job: pre-run -> full refund (runtime 0);
// running -> bill elapsed.
func (bd *Budget) Cancel(jobID string, elapsed, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	e, ok := bd.escrows[jobID]
	if !ok {
		return ErrNoSuchJob
	}
	if err := bd.logCmd(Command{Kind: KindCancel, JobID: jobID, Elapsed: elapsed, Now: now}); err != nil {
		return err
	}
	if !e.Started {
		return bd.settle(jobID, 0, false, now)
	}
	return bd.settle(jobID, elapsed, false, now)
}

// InfraFail settles a NODE_FAIL / PREEMPT. The BillInfraFailures flag decides
// whether the elapsed time is billed to the user or written off by the system.
func (bd *Budget) InfraFail(jobID string, elapsed, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	// BillInfraFailures: true -> bill user (writeOff=false); false -> write off.
	if _, ok := bd.escrows[jobID]; !ok {
		return ErrNoSuchJob
	}
	if err := bd.logCmd(Command{Kind: KindInfraFail, JobID: jobID, Elapsed: elapsed, Now: now}); err != nil {
		return err
	}
	return bd.settle(jobID, elapsed, !bd.BillInfraFailures, now)
}

// Lapse closes the period: no new submits accepted; live escrows stay funded
// and settle normally (refunds land in the now-lapsed pot).
func (bd *Budget) Lapse() {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	_ = bd.logCmd(Command{Kind: KindLapse})
	bd.status = Lapsed
}

// ---- read-only inspectors (take the lock to read a consistent snapshot) ----

// Balance returns the current available balance B.
func (bd *Budget) Balance() Units {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	return bd.B
}

// Live returns the number of live (unsettled) 1:1 escrows.
func (bd *Budget) Live() int {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	return len(bd.escrows)
}

// ConservationOK asserts the single invariant:
//
//	B0 == B + ReservedTotal + Consumed + WriteOff
//
// Every dollar is in exactly one place at all times.
func (bd *Budget) ConservationOK() (bool, Units) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	sum := bd.B + bd.ReservedTotal + bd.Consumed + bd.WriteOff
	return sum == bd.B0, sum
}
