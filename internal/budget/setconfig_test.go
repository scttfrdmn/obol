package budget

import (
	"errors"
	"testing"
)

// TestSetRateFutureOnly confirms set-rate changes the flat rate for FUTURE
// flat-rate submits, leaves live escrows (which froze their own rate) untouched,
// and does not disturb the money ledger.
func TestSetRateFutureOnly(t *testing.T) {
	bd := New(1, 100000, 0, 100000) // flat C = 1

	// A live flat-rate job at the OLD rate: cost 1*100 = 100.
	if err := bd.Submit("old", 100, 10); err != nil {
		t.Fatal(err)
	}
	// Change the flat rate to 5.
	if err := bd.SetRate(5, 20); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	if bd.Report(20).C != 5 {
		t.Errorf("C after set-rate = %d, want 5", bd.Report(20).C)
	}
	// The live "old" escrow is unaffected — settling it bills at the old rate 1.
	if err := bd.Complete("old", 100, 120); err != nil {
		t.Fatal(err)
	}
	if r := bd.Report(120); r.Consumed != 100 { // 1*100, NOT 5*100
		t.Errorf("live escrow billed at %d, want 100 (old rate, not retroactive)", r.Consumed)
	}
	// A NEW flat-rate job uses the new rate 5: cost 5*100 = 500.
	if err := bd.Submit("new", 100, 130); err != nil {
		t.Fatal(err)
	}
	// balance = 100000 - 100(old consumed) - 500(new reserved)
	if bd.Balance() != 100000-100-500 {
		t.Errorf("balance = %d, want %d", bd.Balance(), 100000-100-500)
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation broken after set-rate")
	}
}

// TestSetRateRejectsNonPositive covers the guard and confirms the ledger is
// untouched on rejection.
func TestSetRateRejectsNonPositive(t *testing.T) {
	bd := New(3, 100000, 0, 100000)
	for _, c := range []Seconds{0, -1} {
		if err := bd.SetRate(c, 10); err == nil {
			t.Errorf("SetRate(%d) should be rejected", c)
		}
	}
	if bd.Report(10).C != 3 {
		t.Errorf("C changed by a rejected set-rate: %d", bd.Report(10).C)
	}
}

// TestSetWindowGatesFutureSubmits confirms set-window changes what the gate
// admits, and that a live escrow settles normally across the change.
func TestSetWindowGatesFutureSubmits(t *testing.T) {
	bd := New(1, 100000, 0, 1000) // window [0,1000)

	bd.Submit("live", 100, 10) // admitted, in window
	// Shrink the window to [0,50): now-500 is outside.
	if err := bd.SetWindow(0, 50, 20); err != nil {
		t.Fatalf("SetWindow: %v", err)
	}
	r := bd.Report(20)
	if r.TS != 0 || r.TE != 50 {
		t.Errorf("window = [%d,%d), want [0,50)", r.TS, r.TE)
	}
	// A submit at now=500 is now outside the window -> lapsed/rejected.
	if err := bd.Submit("late", 10, 500); !errors.Is(err, ErrLapsed) {
		t.Errorf("submit outside new window = %v, want ErrLapsed", err)
	}
	// The live escrow still settles normally despite the shrunk window.
	if err := bd.Complete("live", 40, 500); err != nil {
		t.Fatalf("live escrow should settle across a window change: %v", err)
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation broken after set-window")
	}
	// Widen again and confirm submits are admitted.
	if err := bd.SetWindow(0, 100000, 600); err != nil {
		t.Fatal(err)
	}
	if err := bd.Submit("after", 10, 700); err != nil {
		t.Errorf("submit inside widened window failed: %v", err)
	}
}

// TestSetWindowRejectsInverted covers the ts<te guard.
func TestSetWindowRejectsInverted(t *testing.T) {
	bd := New(1, 100000, 0, 1000)
	if err := bd.SetWindow(500, 500, 10); err == nil {
		t.Error("SetWindow(ts==te) should be rejected")
	}
	if err := bd.SetWindow(600, 500, 10); err == nil {
		t.Error("SetWindow(ts>te) should be rejected")
	}
	if r := bd.Report(10); r.TS != 0 || r.TE != 1000 {
		t.Errorf("window changed by a rejected set-window: [%d,%d)", r.TS, r.TE)
	}
}

// TestSetConfigDurableRecovery interleaves config changes with submits, crashes,
// and confirms replay reproduces the rate/window IN ORDER (the #8 guarantee): a
// submit before a set-rate replays at the old rate, one after at the new.
func TestSetConfigDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.Submit("a", 100, 10) // flat rate 1 -> cost 100
	bd.SetRate(4, 20)       // new flat rate 4
	bd.Submit("b", 100, 30) // flat rate 4 -> cost 400
	bd.SetWindow(0, 200000, 40)
	before := fingerprint(bd)
	balBefore := bd.Balance()
	bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if rec.Balance() != balBefore {
		t.Errorf("recovered balance %d != %d (ordering/WAL fields wrong)", rec.Balance(), balBefore)
	}
	r := rec.Report(40)
	if r.C != 4 || r.TE != 200000 {
		t.Errorf("recovered config C=%d TE=%d, want C=4 TE=200000", r.C, r.TE)
	}
	if ok, _ := rec.ConservationOK(); !ok {
		t.Error("recovered conservation broken")
	}
}
