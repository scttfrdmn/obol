package budget

import (
	"testing"
)

// TestSweepUnboundReclaimsStale: an escrow minted at submit but never started
// (Started==false), older than the TTL, is swept with a FULL refund.
func TestSweepUnboundReclaimsStale(t *testing.T) {
	bd := New(1, 1000, 0, 100000)
	if err := bd.SubmitAt("tok-a", 1, 100, 10); err != nil { // cost 100, submitted at t=10
		t.Fatal(err)
	}
	if bd.Balance() != 900 {
		t.Fatalf("balance after submit = %d, want 900", bd.Balance())
	}
	// At t=10+ttl the escrow is stale (never started). ttl=300.
	swept := bd.SweepUnbound(300, 310)
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	if bd.Balance() != 1000 {
		t.Errorf("balance after sweep = %d, want 1000 (full refund)", bd.Balance())
	}
	if bd.Live() != 0 {
		t.Errorf("escrow not removed: live = %d", bd.Live())
	}
	mustConserve(t, bd)
}

// TestSweepUnboundKeepsRecent: a never-started escrow younger than the TTL is a
// legitimately-pending job and must NOT be swept.
func TestSweepUnboundKeepsRecent(t *testing.T) {
	bd := New(1, 1000, 0, 100000)
	bd.SubmitAt("tok-a", 1, 100, 10) // submitted at t=10
	// At t=200, age is 190 < ttl 300 → kept.
	if swept := bd.SweepUnbound(300, 200); swept != 0 {
		t.Fatalf("recent escrow swept = %d, want 0", swept)
	}
	if bd.Live() != 1 || bd.Balance() != 900 {
		t.Errorf("recent escrow disturbed: live=%d balance=%d", bd.Live(), bd.Balance())
	}
	mustConserve(t, bd)
}

// TestSweepUnboundIgnoresStarted: a STARTED escrow is bound/live and is never
// swept by the unbound janitor, even long past the TTL — that is the jobid-based
// SweepOrphans's job, not this one.
func TestSweepUnboundIgnoresStarted(t *testing.T) {
	bd := New(1, 1000, 0, 100000)
	bd.SubmitAt("tok-a", 1, 100, 10)
	if err := bd.Start("tok-a", 20); err != nil { // now bound/running
		t.Fatal(err)
	}
	if swept := bd.SweepUnbound(300, 100000); swept != 0 {
		t.Fatalf("started escrow swept = %d, want 0", swept)
	}
	if bd.Live() != 1 {
		t.Errorf("started escrow removed by unbound sweep: live = %d", bd.Live())
	}
	mustConserve(t, bd)
}

// TestSweepUnboundBoundaryAge: age exactly == ttl is swept (>= boundary).
func TestSweepUnboundBoundaryAge(t *testing.T) {
	bd := New(1, 1000, 0, 100000)
	bd.SubmitAt("tok-a", 1, 100, 10)
	if swept := bd.SweepUnbound(300, 310); swept != 1 { // 310-10 == 300 == ttl
		t.Fatalf("boundary-age escrow swept = %d, want 1", swept)
	}
}

// TestSweepUnboundArrayNoneStarted: an array whose tasks never started, past the
// TTL, is fully reclaimed.
func TestSweepUnboundArrayNoneStarted(t *testing.T) {
	bd := New(1, 100000, 0, 1000000)
	if err := bd.SubmitArrayAt("arr", 1, 5, 100, 10); err != nil { // 5 tasks, cost 5*100=500
		t.Fatal(err)
	}
	before := bd.Balance()
	if before != 100000-500 {
		t.Fatalf("balance after array submit = %d, want %d", before, 100000-500)
	}
	swept := bd.SweepUnbound(300, 400) // age 390 >= 300, no task started
	if swept != 5 {
		t.Fatalf("array tasks swept = %d, want 5", swept)
	}
	if bd.Balance() != 100000 {
		t.Errorf("balance after array sweep = %d, want 100000 (full refund)", bd.Balance())
	}
	mustConserve(t, bd)
}

// TestSweepUnboundArraySomeStarted: an array with at least one started task is
// live work and is left untouched by the unbound sweep.
func TestSweepUnboundArraySomeStarted(t *testing.T) {
	bd := New(1, 100000, 0, 1000000)
	bd.SubmitArrayAt("arr", 1, 5, 100, 10)
	if err := bd.StartTask("arr", 0, 20); err != nil { // one task dispatched
		t.Fatal(err)
	}
	if swept := bd.SweepUnbound(300, 100000); swept != 0 {
		t.Fatalf("partially-started array swept = %d, want 0", swept)
	}
	mustConserve(t, bd)
}

// TestSweepUnboundDurableRecovery: the Submitted timestamp must survive a crash,
// or a recovered escrow would look freshly-submitted and never age out. Submit,
// recover, then sweep with a `now` past the ORIGINAL submit time + ttl.
func TestSweepUnboundDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 1000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.SubmitAt("tok-a", 1, 100, 10) // submitted at t=10
	_ = bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	// If Submitted didn't survive (defaulted to 0), age at t=310 would be 310 and
	// still swept — so to prove the field recovered, sweep at t=200 (age from the
	// real submit=10 is 190 < 300 → keep; age from a lost submit=0 would be 200,
	// still < 300, ambiguous). Use a tighter check: sweep at t=309 with ttl 300.
	// real age = 299 < 300 → KEEP. If Submitted were lost (0), age = 309 >= 300 →
	// wrongly swept. So a kept escrow proves the timestamp recovered.
	if swept := rec.SweepUnbound(300, 309); swept != 0 {
		t.Fatalf("recovered escrow swept at age 299 = %d, want 0 (Submitted not restored?)", swept)
	}
	// And one second later it legitimately ages out.
	if swept := rec.SweepUnbound(300, 310); swept != 1 {
		t.Fatalf("recovered escrow swept at age 300 = %d, want 1", swept)
	}
	mustConserve(t, rec)
}
