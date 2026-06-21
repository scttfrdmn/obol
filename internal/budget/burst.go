package budget

// Burst is an explicit token bucket: idle pace (r0 - aggregate burn) banks
// PERMISSION tokens; a job pushing aggregate burn above r0 reserves excess*w
// tokens at submit and refunds the unused tail at settle. Tokens are not money;
// money conservation is a separate, independent invariant.
//
// Fixed-point r0: r0 = B0/T is generally fractional. Banking uses an exact
// remainder accumulator (fracAcc, numerator over T) so long idles don't drift.
// Excess is computed against floor(r0), which over-reserves by < 1 unit/sec
// (conservative: never under-reserves permission) and keeps the math overflow-safe.

func (bd *Budget) window() Seconds { return bd.TE - bd.TS }
func (bd *Budget) r0Floor() Units  { return bd.B0 / bd.window() } // floor of sustainable rate
func (bd *Budget) burstCeiling() Units {
	return Units(bd.BurstCeilingPct * float64(bd.B0))
}

// accrue fast-forwards the bucket to `now`. Only unused pace banks. The unused
// pace numerator is (B0 - rLive*T); dividing by T gives r0 - rLive. We carry the
// remainder in fracAcc so banking is exact across many calls.
func (bd *Budget) accrue(now Seconds) {
	if !bd.BurstEnabled {
		return
	}
	dt := now - bd.lastTouch
	if dt <= 0 {
		bd.lastTouch = now
		return
	}
	T := bd.window()
	unusedNum := bd.B0 - bd.rLive*T // = (r0 - rLive) * T
	if unusedNum > 0 {
		bd.fracAcc += unusedNum * dt // exact scaled accrual
		carry := bd.fracAcc / T
		bd.fracAcc -= carry * T
		bd.burstPot += carry
		if c := bd.burstCeiling(); bd.burstPot >= c {
			bd.burstPot = c
			bd.fracAcc = 0 // at ceiling: drop sub-unit remainder, prevents unbounded fracAcc
		}
	}
	bd.lastTouch = now
}

// burstReserveForRate computes tokens a job/task at burn rate c reserves given
// current rLive. last-arriver-pays: excess = new - max(old, floor(r0)).
func (bd *Budget) burstReserveForRate(c Units, w Seconds) Units {
	oldR := bd.rLive
	newR := bd.rLive + c
	base := oldR
	if r0f := bd.r0Floor(); base < r0f {
		base = r0f
	}
	excess := newR - base
	if excess < 0 {
		excess = 0
	}
	return excess * w
}

// burstReserveFor is the 1:1 case at the budget's own rate C.
func (bd *Budget) burstReserveFor(w Seconds) Units {
	return bd.burstReserveForRate(bd.C, w)
}

// BurstSnapshot for inspectors / `show`.
func (bd *Budget) BurstSnapshot(now Seconds) (pot, ceiling, rLive Units) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.accrue(now)
	return bd.burstPot, bd.burstCeiling(), bd.rLive
}

// BurstBoundsOK asserts the burst invariant: 0 <= burstPot <= ceiling,
// rLive >= 0, fracAcc in [0, T), and no negative live reservations.
func (bd *Budget) BurstBoundsOK() bool {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if bd.burstPot < 0 || bd.burstPot > bd.burstCeiling() {
		return false
	}
	if bd.rLive < 0 {
		return false
	}
	if bd.fracAcc < 0 || bd.fracAcc >= bd.window() {
		return false
	}
	for _, e := range bd.escrows {
		if e.BurstResv < 0 {
			return false
		}
	}
	return true
}
