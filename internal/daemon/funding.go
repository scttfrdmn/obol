package daemon

import "github.com/scttfrdmn/obol/internal/budget"

// Multi-source funding plan (issue #54). A job may name an ordered list of
// account budgets to fund it (ordered fallback: drain the first, spill to the
// next). The gate splits the job's walltime across those sources so that each
// source funds a CONTIGUOUS TIME SLICE of the job at the full cost rate c:
// source 1 funds the first w_1 seconds, source 2 the next w_2, and so on, with
// Σ w_i = W (the requested walltime).
//
// Why time slices and not cost slices: the kernel escrow is (rate c, walltime w)
// with cost = c*w and refunds computed as c*runtime, so a leg must be an ordinary
// escrow whose reserved amount is exactly c*w_i for an INTEGER w_i. A source's
// balance need not divide by c, so each source funds only the whole seconds it
// can afford — floor(B_i / c) — and its sub-c remainder simply stays in its
// balance (not stranded; just unused by this job). This makes every leg a plain
// single-budget escrow, so settle/refund/reprice/burst work per leg with no
// kernel change, and each budget keeps its own conservation invariant.
//
// The time-sequenced order also gives the right economics: an early completion
// refunds the TAIL legs (later sources) and bills the HEAD legs (the sources that
// funded the seconds actually consumed).

// legPlan is one funded slice of a multi-source job: source Index funds W seconds
// at the job's rate, reserving Cost = c*W in that source's budget.
type legPlan struct {
	Index int            // position in the caller's ordered sources slice
	W     budget.Seconds // whole seconds this source funds
	Cost  budget.Units   // c * W, the amount reserved in this source
}

// fundingPlan computes the ordered-fallback split of a job (rate c, walltime w)
// across sources with the given available balances (same order as the sources).
// It returns the legs that carry a non-zero slice and whether the job is fully
// fundable (Σ floor(balance_i/c) >= w). When not fundable, the returned legs are
// the partial fill (useful for a diagnostic), but callers must treat fundable ==
// false as a hard reject and place nothing.
//
// A source that cannot fund a whole second (balance < c) contributes no leg. The
// rate c must be > 0 and w > 0 (the gate guarantees both before calling).
func fundingPlan(balances []budget.Units, c, w budget.Seconds) (legs []legPlan, fundable bool) {
	if c <= 0 || w <= 0 {
		return nil, false
	}
	need := w
	for i, b := range balances {
		if need == 0 {
			break
		}
		capacity := b / c // whole seconds source i can fund at rate c (floor)
		if capacity <= 0 {
			continue // this source can't fund a whole second; skip it
		}
		wi := capacity
		if wi > need {
			wi = need
		}
		legs = append(legs, legPlan{Index: i, W: wi, Cost: c * wi})
		need -= wi
	}
	return legs, need == 0
}

// legRuntime apportions a job's total runtime across the plan's contiguous time
// slices: leg i (funding seconds [prefix, prefix+W_i)) sees runtime
// clamp(runtime - prefix, 0, W_i). Summed over all legs this equals the job's
// runtime (capped at W), so Σ (c * legRuntime_i) == c * runtime — the total billed
// equals the job's true cost, and each budget conserves independently.
func legRuntime(legs []legPlan, idx int, runtime budget.Seconds) budget.Seconds {
	if runtime < 0 {
		runtime = 0
	}
	var prefix budget.Seconds
	for i := 0; i < idx; i++ {
		prefix += legs[i].W
	}
	r := runtime - prefix
	if r < 0 {
		r = 0
	}
	if r > legs[idx].W {
		r = legs[idx].W
	}
	return r
}
