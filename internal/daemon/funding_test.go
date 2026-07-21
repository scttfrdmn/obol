package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
)

// TestFundingPlanSingleSourceEquivalence: one source that fully covers the job
// yields a single leg identical to the single-source cost (back-compat sanity).
func TestFundingPlanSingleSourceEquivalence(t *testing.T) {
	legs, ok := fundingPlan([]budget.Units{10000}, 10, 100) // cost 1000 <= 10000
	if !ok {
		t.Fatal("single ample source should be fundable")
	}
	if len(legs) != 1 || legs[0].W != 100 || legs[0].Cost != 1000 {
		t.Fatalf("legs = %+v, want one leg W=100 cost=1000", legs)
	}
}

// TestFundingPlanOrderedFallback: source 1 is drained to a whole-second boundary,
// the remainder spills to source 2.
func TestFundingPlanOrderedFallback(t *testing.T) {
	// c=10, W=100 → total cost 1000. Source 1 has 350 → floor(350/10)=35 whole
	// seconds (cost 350; its extra 0 is exact here). Source 2 covers 65s (650).
	legs, ok := fundingPlan([]budget.Units{350, 100000}, 10, 100)
	if !ok {
		t.Fatal("should be fundable across two sources")
	}
	if len(legs) != 2 {
		t.Fatalf("want 2 legs, got %+v", legs)
	}
	if legs[0].W != 35 || legs[0].Cost != 350 {
		t.Errorf("leg 0 = %+v, want W=35 cost=350", legs[0])
	}
	if legs[1].W != 65 || legs[1].Cost != 650 {
		t.Errorf("leg 1 = %+v, want W=65 cost=650", legs[1])
	}
	// Σ W == the job walltime; Σ cost == c*W.
	if legs[0].W+legs[1].W != 100 || legs[0].Cost+legs[1].Cost != 1000 {
		t.Errorf("legs don't sum to the job: %+v", legs)
	}
}

// TestFundingPlanQuantizesSubRate: a source whose balance is not a multiple of c
// funds only floor(B/c) whole seconds; the sub-c remainder is left in the source.
func TestFundingPlanQuantizesSubRate(t *testing.T) {
	// c=10, source 1 has 355 → floor(355/10)=35 seconds (350 reserved); the extra
	// 5 units are NOT used by this job. Source 2 funds the rest.
	legs, ok := fundingPlan([]budget.Units{355, 100000}, 10, 100)
	if !ok {
		t.Fatal("fundable")
	}
	if legs[0].W != 35 || legs[0].Cost != 350 {
		t.Errorf("leg 0 = %+v, want W=35 cost=350 (5 units left unused in source)", legs[0])
	}
	if legs[1].W != 65 {
		t.Errorf("leg 1 W = %d, want 65", legs[1].W)
	}
}

// TestFundingPlanSkipsSubRateSource: a source with balance < c funds zero whole
// seconds and contributes no leg (it is skipped, not an error).
func TestFundingPlanSkipsSubRateSource(t *testing.T) {
	// c=10; source 1 has 7 (< c) → skipped; source 2 covers the whole job.
	legs, ok := fundingPlan([]budget.Units{7, 100000}, 10, 100)
	if !ok {
		t.Fatal("fundable via source 2")
	}
	if len(legs) != 1 || legs[0].Index != 1 || legs[0].W != 100 {
		t.Fatalf("want a single leg from source index 1, got %+v", legs)
	}
}

// TestFundingPlanExactFit: sources sum to exactly the job's whole-second capacity.
func TestFundingPlanExactFit(t *testing.T) {
	// c=10, W=100 → need 100 seconds. Sources fund 40 + 60 = 100 exactly.
	legs, ok := fundingPlan([]budget.Units{400, 600}, 10, 100)
	if !ok {
		t.Fatal("exact fit should be fundable")
	}
	if len(legs) != 2 || legs[0].W != 40 || legs[1].W != 60 {
		t.Fatalf("want 40+60, got %+v", legs)
	}
}

// TestFundingPlanNotFundable: capacity one whole second short → reject.
func TestFundingPlanNotFundable(t *testing.T) {
	// c=10, W=100 → need 100 seconds. Sources fund floor(495/10)+floor(495/10) =
	// 49+49 = 98 seconds < 100 → not fundable.
	legs, ok := fundingPlan([]budget.Units{495, 495}, 10, 100)
	if ok {
		t.Fatalf("should NOT be fundable (98s < 100s), got legs %+v", legs)
	}
}

// TestFundingPlanBoundaryOneShort: Σ floor(B/c) == W-1 rejects; +c makes it fund.
func TestFundingPlanBoundaryOneShort(t *testing.T) {
	// c=5, W=20 → need 20 seconds. 45+50 → floor = 9+10 = 19 == W-1 → reject.
	if _, ok := fundingPlan([]budget.Units{45, 50}, 5, 20); ok {
		t.Error("19s < 20s should reject")
	}
	// Bump source 1 by one whole second's worth (5) → 50 → floor 10 → 10+10 = 20.
	if _, ok := fundingPlan([]budget.Units{50, 50}, 5, 20); !ok {
		t.Error("20s == 20s should fund")
	}
}

// TestFundingPlanRejectsBadArgs: non-positive rate or walltime is never fundable.
func TestFundingPlanRejectsBadArgs(t *testing.T) {
	if _, ok := fundingPlan([]budget.Units{1000}, 0, 100); ok {
		t.Error("c=0 should not be fundable")
	}
	if _, ok := fundingPlan([]budget.Units{1000}, 10, 0); ok {
		t.Error("w=0 should not be fundable")
	}
	if _, ok := fundingPlan(nil, 10, 100); ok {
		t.Error("no sources should not be fundable")
	}
}

// TestLegRuntimeApportionment verifies the settle-time split: Σ legRuntime ==
// min(runtime, W), so Σ billed == c*runtime, with early completion refunding the
// tail legs and billing the head legs.
func TestLegRuntimeApportionment(t *testing.T) {
	legs, ok := fundingPlan([]budget.Units{350, 100000}, 10, 100) // legs: 35s, 65s
	if !ok {
		t.Fatal("fundable")
	}
	// Job ran 50 of 100 seconds. Leg 0 (first 35s) fully consumed; leg 1 (next
	// 65s) consumed 15 of its 65.
	if r := legRuntime(legs, 0, 50); r != 35 {
		t.Errorf("leg 0 runtime = %d, want 35 (fully within the first slice)", r)
	}
	if r := legRuntime(legs, 1, 50); r != 15 {
		t.Errorf("leg 1 runtime = %d, want 15 (50-35)", r)
	}
	// Σ == runtime.
	if legRuntime(legs, 0, 50)+legRuntime(legs, 1, 50) != 50 {
		t.Error("leg runtimes must sum to the job runtime")
	}
	// Full completion: every leg fully consumed.
	if legRuntime(legs, 0, 100) != 35 || legRuntime(legs, 1, 100) != 65 {
		t.Error("full completion should consume every leg's whole slice")
	}
	// Runtime beyond W clamps (Timeout passes W): still sums to W.
	if legRuntime(legs, 0, 100000) != 35 || legRuntime(legs, 1, 100000) != 65 {
		t.Error("over-W runtime must clamp per leg")
	}
	// Very early completion: only the head leg bills, the tail refunds fully.
	if legRuntime(legs, 0, 10) != 10 || legRuntime(legs, 1, 10) != 0 {
		t.Error("early completion should bill only the head leg")
	}
}
