package budget

// ---------------------------------------------------------------------------
// 1:N array layer. One submission == one escrow holding N tasks that share a
// frozen walltime w. Debited once (all-or-nothing) as N*c*w; each task settles
// independently, drawing its own c*w slice and refunding its own tail.
//
// Per-escrow invariant (what global conservation cannot see):
//
//	reserved == remaining*c*w + sliceRefunded
//
// where sliceRefunded is the running sum of tails returned by settled tasks of
// THIS escrow. A task double-settling or settling the wrong slice keeps the
// global books balanced but breaks this. So we assert it after every task.
// ---------------------------------------------------------------------------

// ArrayEscrow generalizes Escrow. The 1:1 Escrow is the N=1 case; we model
// arrays explicitly so per-task drawdown and partial completion are first-class.
type ArrayEscrow struct {
	ArrayID   string
	C         Units   // copied from budget at submit (frozen)
	W         Seconds // frozen walltime, shared by all tasks
	N         int     // original task count
	Remaining int     // tasks not yet settled
	Reserved  Units   // live reservation for this array == Remaining*C*W (+ in-flight)
	tasks     map[int]*taskState
}

type taskState struct {
	idx       int
	started   bool
	settled   bool
	burstResv Units // tokens reserved when this task dispatched
}

func (a *ArrayEscrow) slice() Units { return a.C * a.W }

// arrays lives alongside the 1:1 escrows map. We keep them separate so the
// proven 1:1 paths are untouched; SubmitArray/settleTask operate here.
func (bd *Budget) ensureArrays() {
	if bd.arrays == nil {
		bd.arrays = make(map[string]*ArrayEscrow)
	}
}

// SubmitArray is the array gate: all-or-nothing debit of N*c*w under one lock.
func (bd *Budget) SubmitArray(arrayID string, n int, w Seconds, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	defer bd.publishLocked() // refresh tier-2 read view
	bd.ensureArrays()

	if bd.status != Active {
		return ErrLapsed
	}
	if now < bd.TS || now >= bd.TE {
		return ErrLapsed
	}
	if _, dup := bd.arrays[arrayID]; dup {
		return ErrBadState
	}
	if n <= 0 || w <= 0 {
		return ErrBadState
	}
	cost := bd.C * w * Units(n) // N*c*w
	if cost > bd.B {            // solvency, whole array
		return ErrInsufficient
	}
	if !bd.rateOK(cost, now) { // burst ceiling sees the whole array's cost
		return ErrRateExceeded
	}
	if err := bd.logCmd(Command{Kind: KindSubmitArray, ArrayID: arrayID, N: n, W: w, Now: now}); err != nil {
		return err
	}
	bd.B -= cost
	bd.ReservedTotal += cost

	ae := &ArrayEscrow{
		ArrayID: arrayID, C: bd.C, W: w, N: n, Remaining: n,
		Reserved: cost, tasks: make(map[int]*taskState, n),
	}
	for i := 0; i < n; i++ {
		ae.tasks[i] = &taskState{idx: i}
	}
	bd.arrays[arrayID] = ae
	return nil
}

// StartTask marks one array task as dispatched (pending->running), running the
// per-task burst dispatch gate when burst is enabled. A task that cannot reserve
// burst headroom is rejected so the caller can retry later.
func (bd *Budget) StartTask(arrayID string, idx int, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	defer bd.publishLocked() // refresh tier-2 read view
	ae, ok := bd.arrays[arrayID]
	if !ok {
		return ErrNoSuchJob
	}
	ts, ok := ae.tasks[idx]
	if !ok || ts.settled {
		return ErrBadState
	}
	if ts.started {
		return nil
	}
	// Per-task burst dispatch gate. Each running task burns at the array's
	// frozen rate ae.C; last-arriver-pays against current aggregate rLive.
	if bd.BurstEnabled {
		bd.accrue(now)
		resv := bd.burstReserveForRate(ae.C, ae.W)
		if resv > 0 {
			if bd.BurstDrawCap > 0 && resv > bd.BurstDrawCap {
				return ErrBurstDrawCap
			}
			if resv > bd.burstPot {
				return ErrBurstInsuff
			}
			if err := bd.logCmd(Command{Kind: KindStartTask, ArrayID: arrayID, Idx: idx, Now: now}); err != nil {
				return err
			}
			bd.burstPot -= resv
			ts.burstResv = resv
			bd.rLive += ae.C
			ts.started = true
			return nil
		}
		bd.rLive += ae.C
	}
	if err := bd.logCmd(Command{Kind: KindStartTask, ArrayID: arrayID, Idx: idx, Now: now}); err != nil {
		return err
	}
	ts.started = true
	return nil
}

// settleTask is the per-task analogue of settle. It draws one slice (c*w) off
// the array escrow, bills/writes-off the used part, refunds the tail, and
// decrements remaining. Closes the escrow when the last task settles.
func (bd *Budget) settleTask(arrayID string, idx int, runtime Seconds, writeOff bool, now Seconds) error {
	ae, ok := bd.arrays[arrayID]
	if !ok {
		return ErrNoSuchJob
	}
	ts, ok := ae.tasks[idx]
	if !ok {
		return ErrNoSuchJob
	}
	if ts.settled {
		return ErrBadState // double-settle guard — the thing that would silently corrupt
	}
	if bd.BurstEnabled {
		bd.accrue(now) // fast-forward with this task still counted in rLive
	}

	if runtime < 0 {
		runtime = 0
	}
	if runtime > ae.W {
		runtime = ae.W
	}
	slice := ae.slice()
	used := ae.C * runtime
	refund := slice - used // c*(w-runtime) >= 0

	// Mutate global books.
	bd.ReservedTotal -= slice
	bd.B += refund
	if writeOff {
		bd.WriteOff += used
	} else {
		bd.Consumed += used
	}

	// Burst: if the task was dispatched, drop its burn from rLive and refund the
	// unused token tail.
	if bd.BurstEnabled && ts.started {
		bd.rLive -= ae.C
		if bd.rLive < 0 {
			bd.rLive = 0
		}
		if ts.burstResv > 0 && ae.W > 0 {
			refundTok := ts.burstResv * (ae.W - runtime) / ae.W
			bd.burstPot += refundTok
			if c := bd.burstCeiling(); bd.burstPot > c {
				bd.burstPot = c
			}
		}
	}

	// Mutate per-escrow books.
	ae.Reserved -= slice
	ae.Remaining--
	ts.settled = true

	// Per-escrow invariant: reserved must now equal remaining whole slices.
	if ae.Reserved != Units(ae.Remaining)*slice {
		// Should be impossible; surfaced as a hard failure for the harness.
		panic("per-escrow invariant broken")
	}

	if ae.Remaining == 0 {
		delete(bd.arrays, arrayID)
	}
	bd.publishLocked() // shared task-settle core: refreshes the tier-2 view
	return nil
}

// CompleteTask settles a clean per-task exit (runtime<=w), billing the used
// slice and refunding its tail.
func (bd *Budget) CompleteTask(arrayID string, idx int, runtime, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if err := bd.logTaskCmd(KindCompleteTask, arrayID, idx, runtime, 0, now); err != nil {
		return err
	}
	return bd.settleTask(arrayID, idx, runtime, false, now)
}

// TimeoutTask settles a task that hit its walltime: runtime==w, refund 0.
func (bd *Budget) TimeoutTask(arrayID string, idx int, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	ae, ok := bd.arrays[arrayID]
	if !ok {
		return ErrNoSuchJob
	}
	if err := bd.logTaskCmd(KindTimeoutTask, arrayID, idx, 0, 0, now); err != nil {
		return err
	}
	return bd.settleTask(arrayID, idx, ae.W, false, now)
}

// CancelTask settles a cancelled task: pre-run -> full slice refund;
// running -> bill elapsed.
func (bd *Budget) CancelTask(arrayID string, idx int, elapsed, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	ae, ok := bd.arrays[arrayID]
	if !ok {
		return ErrNoSuchJob
	}
	ts, ok := ae.tasks[idx]
	if !ok {
		return ErrNoSuchJob
	}
	if err := bd.logTaskCmd(KindCancelTask, arrayID, idx, 0, elapsed, now); err != nil {
		return err
	}
	if !ts.started {
		return bd.settleTask(arrayID, idx, 0, false, now)
	}
	return bd.settleTask(arrayID, idx, elapsed, false, now)
}

// InfraFailTask settles a task infra failure; the BillInfraFailures flag
// decides bill vs write-off, per task.
func (bd *Budget) InfraFailTask(arrayID string, idx int, elapsed, now Seconds) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if _, ok := bd.arrays[arrayID]; !ok {
		return ErrNoSuchJob
	}
	if err := bd.logTaskCmd(KindInfraFailTask, arrayID, idx, 0, elapsed, now); err != nil {
		return err
	}
	return bd.settleTask(arrayID, idx, elapsed, !bd.BillInfraFailures, now)
}

// logTaskCmd is a small helper for the array exit commands.
func (bd *Budget) logTaskCmd(kind, arrayID string, idx int, runtime, elapsed, now Seconds) error {
	return bd.logCmd(Command{Kind: kind, ArrayID: arrayID, Idx: idx, Runtime: runtime, Elapsed: elapsed, Now: now})
}

// ---- array-aware conservation: global books still balance, AND every live
// array satisfies its own per-escrow equality. ----

// ConservationOKArrays asserts global conservation AND every live array's
// per-escrow equality (reserved == remaining*c*w). Returns the observed sum.
func (bd *Budget) ConservationOKArrays() (bool, Units) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	sum := bd.B + bd.ReservedTotal + bd.Consumed + bd.WriteOff
	if sum != bd.B0 {
		return false, sum
	}
	for _, ae := range bd.arrays {
		if ae.Reserved != Units(ae.Remaining)*ae.slice() {
			return false, sum
		}
	}
	return true, sum
}

// LiveArrays returns the number of live (unsettled) array escrows.
func (bd *Budget) LiveArrays() int {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	return len(bd.arrays)
}
