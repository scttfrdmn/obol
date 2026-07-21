package budget

import (
	"errors"
	"fmt"
	"testing"
)

// dispatchBudget mirrors freshBurst: c=10/s, B0=10000, window [0,1000) so
// r0=10/s. One job at C=10 sits exactly at r0 (no burst); concurrency bursts.
func dispatchBudget() *Budget {
	bd := New(10, 10000, 0, 1000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	bd.publishLocked() // re-publish so the view reflects the burst config just set
	return bd
}

// TestMayDispatchUnderR0 — a job that keeps aggregate burn at/below r0 needs no
// tokens and always dispatches.
func TestMayDispatchUnderR0(t *testing.T) {
	bd := dispatchBudget()
	// First job at C=10 == r0: excess 0.
	v := bd.MayDispatch(10, 50, 0)
	if !v.Dispatch || v.Reserve != 0 {
		t.Fatalf("under-r0 job: %+v, want dispatch with reserve 0", v)
	}
}

// TestMayDispatchNeedsHeadroom — a second concurrent job pushes over r0 and needs
// banked tokens; with headroom it dispatches, once exhausted it holds.
func TestMayDispatchNeedsHeadroom(t *testing.T) {
	bd := dispatchBudget()
	bd.Submit("A", 100, 300)
	bd.Start("A", 300) // rLive=10 (==r0); banked ~3000 by t=300
	// A second job at C=10 for w=100: excess 10*100 = 1000 tokens. Banked 3000 covers.
	v := bd.MayDispatch(10, 100, 300)
	if !v.Dispatch {
		t.Fatalf("with 3000 banked, 1000-token job should dispatch: %+v", v)
	}
	if v.Reserve != 1000 {
		t.Errorf("reserve = %d, want 1000", v.Reserve)
	}
	// A huge job whose reservation exceeds the pot must hold.
	v = bd.MayDispatch(10, 100000, 300) // excess 10 * 100000 = 1e6 >> pot
	if v.Dispatch {
		t.Fatalf("over-pot job should hold: %+v", v)
	}
	if v.Reason != "insufficient burst headroom" {
		t.Errorf("reason = %q, want insufficient burst headroom", v.Reason)
	}
}

// TestMayDispatchDrawCap — a per-job draw cap holds a job whose single
// reservation exceeds it, regardless of bank.
func TestMayDispatchDrawCap(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	bd.BurstDrawCap = 200
	bd.publishLocked()
	bd.Submit("A", 100, 500)
	bd.Start("A", 500) // rLive=10, bank ~5000
	// Excess 10*100 = 1000 > 200 cap.
	v := bd.MayDispatch(10, 100, 500)
	if v.Dispatch || v.Reason != "burst draw cap exceeded" {
		t.Fatalf("draw-cap job should hold: %+v", v)
	}
	// A short job: excess 10*20 = 200 == cap, fits.
	if v := bd.MayDispatch(10, 20, 500); !v.Dispatch {
		t.Fatalf("w=20 job within cap should dispatch: %+v", v)
	}
}

// TestMayDispatchBurstDisabled — with burst off the query always permits (Start
// never gates).
func TestMayDispatchBurstDisabled(t *testing.T) {
	bd := New(10, 10000, 0, 1000) // burst off by default
	if v := bd.MayDispatch(1000000, 1000, 0); !v.Dispatch {
		t.Fatalf("burst-disabled budget should always dispatch: %+v", v)
	}
}

// TestMayDispatchIgnoresLapse — dispatch is a pure burst-headroom question and
// does NOT gate on lifecycle status, because bd.Start doesn't either (the kernel
// lets live escrows start/settle on a lapsed budget). A under-r0 job on a lapsed
// budget still dispatches, matching Start.
func TestMayDispatchIgnoresLapse(t *testing.T) {
	bd := dispatchBudget()
	bd.Lapse()
	// An under-r0 job needs no tokens; Start would succeed on a lapsed budget, so
	// MayDispatch must agree.
	if v := bd.MayDispatch(10, 50, 100); !v.Dispatch {
		t.Fatalf("under-r0 job on lapsed budget should dispatch (Start would): %+v", v)
	}
}

// TestMayDispatchAgreesWithStart is the anti-drift guard: across a matrix of live
// states, the lock-free MayDispatch verdict must equal whether a real Start at the
// same rate/walltime/now would actually succeed. If Start's burst gate ever
// changes and the projection doesn't, this fails.
func TestMayDispatchAgreesWithStart(t *testing.T) {
	// Vary: idle time before the pending job (how much is banked), the pending
	// job's walltime (reservation size), a background running job's rate (rLive),
	// and an optional draw cap.
	idles := []Seconds{0, 50, 200, 500}
	walltimes := []Seconds{10, 50, 100, 400}
	bgRates := []Units{0, 10, 20}
	caps := []Units{0, 200}

	for _, idle := range idles {
		for _, w := range walltimes {
			for _, bg := range bgRates {
				for _, cap := range caps {
					name := fmt.Sprintf("idle=%d/w=%d/bg=%d/cap=%d", idle, w, bg, cap)
					t.Run(name, func(t *testing.T) {
						assertDispatchAgreesWithStart(t, idle, w, bg, cap)
					})
				}
			}
		}
	}
}

// assertDispatchAgreesWithStart builds two identical budgets, queries MayDispatch
// on one and performs the real Start on the other, and asserts they agree.
func assertDispatchAgreesWithStart(t *testing.T, idle, w Seconds, bg, drawCap Units) {
	t.Helper()
	mk := func() *Budget {
		bd := New(10, 10000, 0, 1000)
		bd.BurstEnabled = true
		bd.BurstCeilingPct = 1.0
		bd.BurstDrawCap = drawCap
		bd.publishLocked()
		// Optionally run a background job at rate bg so rLive is nonzero at query time.
		if bg > 0 {
			bd.SubmitAt("bg", bg, 1000, 0)
			bd.Start("bg", 0) // starts at t=0, rLive=bg
		}
		return bd
	}
	now := idle // query/start at t=idle so the bank has accrued (or not)
	c := Units(10)

	// Budget 1: lock-free query.
	q := mk()
	// The pending job must be escrowed so a real Start is possible on budget 2;
	// escrow on the query budget too so both views match exactly.
	q.SubmitAt("p", c, w, now)
	verdict := q.MayDispatch(c, w, now)

	// Budget 2: identical, then the authoritative Start.
	s := mk()
	s.SubmitAt("p", c, w, now)
	startErr := s.Start("p", now)

	started := startErr == nil
	if verdict.Dispatch != started {
		t.Fatalf("MayDispatch=%v but Start success=%v (err=%v); verdict=%+v",
			verdict.Dispatch, started, startErr, verdict)
	}
	// When Start failed on burst grounds, the verdict reason should be a burst hold.
	if !started && startErr != nil {
		if errors.Is(startErr, ErrBurstInsuff) && verdict.Reason != "insufficient burst headroom" {
			t.Errorf("Start said ErrBurstInsuff but verdict reason=%q", verdict.Reason)
		}
		if errors.Is(startErr, ErrBurstDrawCap) && verdict.Reason != "burst draw cap exceeded" {
			t.Errorf("Start said ErrBurstDrawCap but verdict reason=%q", verdict.Reason)
		}
	}
}

// TestMayDispatchReflectsFreshMutations — after a mutation publishes, the
// lock-free query sees the new rLive/pot without any explicit refresh, and the
// view's lastTouch/fracAcc let the pot project forward in time.
func TestMayDispatchReflectsFreshMutations(t *testing.T) {
	bd := dispatchBudget()
	// Background job at C=10 == r0 running from t=0 → rLive=10, nothing banked yet
	// (running exactly at r0 banks nothing). Start publishes lastTouch=0.
	bd.Submit("A", 1000, 0)
	bd.Start("A", 0)
	// A second concurrent job at t=0 pushes over r0 (excess 10*100 = 1000 tokens)
	// but nothing is banked → holds.
	if v := bd.MayDispatch(10, 100, 0); v.Dispatch {
		t.Fatalf("at t=0 with no bank, an over-r0 job should hold: %+v", v)
	}
	// Cancel A so rLive→0 and idle banks; the mutation republishes the view.
	bd.Cancel("A", 0, 0)
	// By t=300, idle banking (r0*300 = 3000) covers a 1000-token job. MayDispatch
	// must see it via the projected pot (lastTouch/fracAcc carried in the view).
	if v := bd.MayDispatch(10, 100, 300); !v.Dispatch {
		t.Fatalf("by t=300 the banked pot should cover a 1000-token job: %+v", v)
	}
}
