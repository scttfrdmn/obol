package budget

import "testing"

// TestSubmitAtPerJobRate confirms cost is computed at the per-job rate, not the
// budget's flat C, and that settle/refund use the same rate.
func TestSubmitAtPerJobRate(t *testing.T) {
	bd := New(1, 100000, 0, 100000) // flat C = 1

	// Submit at c=5 for w=100 -> cost 500 (not 100 at the flat rate).
	if err := bd.SubmitAt("gpu", 5, 100, 10); err != nil {
		t.Fatal(err)
	}
	if bal := bd.Balance(); bal != 100000-500 {
		t.Errorf("after submit@5 balance = %d, want %d", bal, 100000-500)
	}
	// Complete after 40s -> billed 5*40=200, refund 300.
	if err := bd.Complete("gpu", 40, 60); err != nil {
		t.Fatal(err)
	}
	if bal := bd.Balance(); bal != 100000-200 {
		t.Errorf("after complete balance = %d, want %d", bal, 100000-200)
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken: %d", sum)
	}
}

// TestSubmitAtFallback confirms c<=0 uses the budget's flat rate, so SubmitAt is
// a strict superset of Submit.
func TestSubmitAtFallback(t *testing.T) {
	bd := New(7, 100000, 0, 100000) // flat C = 7
	if err := bd.SubmitAt("j", 0, 100, 10); err != nil {
		t.Fatal(err)
	}
	// Falls back to bd.C=7 -> cost 700.
	if bal := bd.Balance(); bal != 100000-700 {
		t.Errorf("fallback cost wrong: balance = %d, want %d", bal, 100000-700)
	}
}

// TestMixedRatesConservation runs several escrows at different per-job rates in
// one budget and confirms conservation holds through a mix of settlements.
func TestMixedRatesConservation(t *testing.T) {
	bd := New(1, 1_000_000, 0, 1_000_000)
	rates := []Seconds{1, 3, 10, 25}
	for i, c := range rates {
		id := "j" + itoa(i)
		if err := bd.SubmitAt(id, c, 100, 10); err != nil {
			t.Fatalf("submit %s@%d: %v", id, c, err)
		}
	}
	// Settle each with a different runtime.
	bd.Complete("j0", 100, 200) // full walltime at c=1  -> billed 100
	bd.Complete("j1", 50, 200)  // half at c=3            -> billed 150
	bd.Cancel("j2", 0, 200)     // unstarted cancel       -> full refund
	bd.Timeout("j3", 200)       // hit walltime at c=25    -> billed 2500

	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken across mixed rates: %d", sum)
	}
	// Consumed = 100 + 150 + 0 + 2500 = 2750.
	if r := bd.Report(200); r.Consumed != 2750 {
		t.Errorf("consumed = %d, want 2750", r.Consumed)
	}
}

// TestSubmitArrayAtPerRate confirms the array gate freezes and bills the per-job
// rate across per-task settlement.
func TestSubmitArrayAtPerRate(t *testing.T) {
	bd := New(1, 1_000_000, 0, 1_000_000)
	// 4 tasks, c=10, w=100 -> reserve 4*10*100 = 4000.
	if err := bd.SubmitArrayAt("arr", 10, 4, 100, 10); err != nil {
		t.Fatal(err)
	}
	if bal := bd.Balance(); bal != 1_000_000-4000 {
		t.Errorf("array reserve wrong: balance = %d, want %d", bal, 1_000_000-4000)
	}
	// Complete one task after 30s -> billed 10*30=300, refund 700.
	if err := bd.CompleteTask("arr", 0, 30, 200); err != nil {
		t.Fatal(err)
	}
	if ok, sum := bd.ConservationOKArrays(); !ok {
		t.Errorf("conservation broken: %d", sum)
	}
}

// TestPerRateDurableRecovery is the replay-path guard: submit at non-default
// rates, crash, recover, and assert balances match — proving the WAL carries C
// and replay reconstructs per-job cost exactly.
func TestPerRateDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 1_000_000, 0, 1_000_000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.SubmitAt("a", 5, 100, 10)        // cost 500
	bd.SubmitAt("b", 20, 50, 20)        // cost 1000
	bd.SubmitArrayAt("c", 3, 2, 40, 30) // 2*3*40 = 240
	bd.Start("a", 40)
	bd.Complete("a", 60, 100) // billed 5*60=300, refund 200
	before := fingerprint(bd)
	balBefore := bd.Balance()
	bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("per-rate state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if rec.Balance() != balBefore {
		t.Errorf("recovered balance %d != %d — WAL did not carry the rate", rec.Balance(), balBefore)
	}
	if ok, sum := rec.ConservationOKArrays(); !ok {
		t.Errorf("recovered conservation broken: %d", sum)
	}
}
