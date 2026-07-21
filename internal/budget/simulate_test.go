package budget

import "testing"

// TestSimulateDoesNotMutate is the core guarantee: Simulate reports a verdict
// without changing any state — balance, escrows, and conservation are identical
// before and after.
func TestSimulateDoesNotMutate(t *testing.T) {
	bd := New(2, 1000, 0, 100000)
	before := fingerprint(bd)

	sim := bd.Simulate(0, 100, 10) // flat rate 2, w 100 -> cost 200 <= 1000
	if !sim.Admit {
		t.Errorf("expected admit, got %+v", sim)
	}
	if sim.Cost != 200 {
		t.Errorf("cost = %d, want 200", sim.Cost)
	}
	if after := fingerprint(bd); before != after {
		t.Errorf("Simulate mutated state:\n before=%s\n after =%s", before, after)
	}
	if bd.Live() != 0 {
		t.Errorf("Simulate created an escrow: live=%d", bd.Live())
	}
}

// TestSimulateVerdicts covers the deny reasons the gate would give.
func TestSimulateVerdicts(t *testing.T) {
	bd := New(1, 500, 0, 1000)

	// Funded.
	if s := bd.Simulate(0, 100, 10); !s.Admit || s.Reason != "" {
		t.Errorf("funded: %+v", s)
	}
	// Insufficient: cost 600 > balance 500.
	if s := bd.Simulate(0, 600, 10); s.Admit || s.Reason == "" {
		t.Errorf("expected insufficient deny, got %+v", s)
	}
	// Outside window (now >= TE).
	if s := bd.Simulate(0, 10, 2000); s.Admit {
		t.Errorf("expected out-of-window deny, got %+v", s)
	}
	// Lapsed.
	bd.Lapse()
	if s := bd.Simulate(0, 10, 10); s.Admit {
		t.Errorf("expected lapsed deny, got %+v", s)
	}
}

// TestSimulateRunway reports time-to-empty at the current balance and rate.
func TestSimulateRunway(t *testing.T) {
	bd := New(10, 1000, 0, 100000) // rate 10, balance 1000 -> runway 100s
	s := bd.Simulate(0, 10, 10)
	if s.Runway != 100 {
		t.Errorf("runway = %d, want 100 (B/C)", s.Runway)
	}
}

// TestSimulateBurstHeadroom checks the dispatch-time burst gate is simulated: a
// job needing more burst tokens than are banked is flagged, without mutating the
// bucket.
func TestSimulateBurstHeadroom(t *testing.T) {
	bd := New(1, 1_000_000, 0, 1000) // r0 = B0/window = 1000000/1000 = 1000/s
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0

	// A job at the budget's rate (c=1) needs no burst (well under r0 floor), so
	// money-funded => admit, and burst headroom is fine.
	s := bd.Simulate(0, 100, 10)
	if !s.Admit {
		t.Errorf("small job should admit: %+v", s)
	}
	// A job whose rate pushes aggregate burn far above r0 needs banked tokens it
	// doesn't have (pot starts at 0). Simulate should flag insufficient burst,
	// and must not have banked/spent anything.
	potBefore, _, _ := bd.BurstSnapshot(10)
	big := bd.Simulate(5000, 100, 10) // c=5000 >> r0 floor 1000 -> needs excess*w tokens
	if big.Admit {
		t.Errorf("burst-starved job should be denied: %+v", big)
	}
	potAfter, _, _ := bd.BurstSnapshot(10)
	if potBefore != potAfter {
		t.Errorf("Simulate changed the burst pot: %d -> %d", potBefore, potAfter)
	}
}
