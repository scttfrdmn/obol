package budget

import (
	"errors"
	"testing"
)

// TestSetBurstEnableBanksFromNow: enabling burst on a budget that's been running
// a while must anchor accrual to `now`, not the window start — otherwise the
// first accrue would instantly bank a huge idle pot. Right after enabling, the
// pot is still ~0.
func TestSetBurstEnableBanksFromNow(t *testing.T) {
	bd := New(10, 10000, 0, 1000) // r0 = 10/s; burst OFF
	// Enable at t=500, halfway through the window.
	if err := bd.SetBurst(true, 1.0, 0, 500); err != nil {
		t.Fatalf("SetBurst enable: %v", err)
	}
	pot, _, _ := bd.BurstSnapshot(500) // no time has passed since enable
	if pot != 0 {
		t.Errorf("pot right after enabling = %d, want 0 (must not bank from window start)", pot)
	}
	// Idling 100s from the enable point banks r0*100 = 1000.
	if pot, _, _ := bd.BurstSnapshot(600); pot != 1000 {
		t.Errorf("pot after 100s idle = %d, want 1000", pot)
	}
	if !bd.BurstBoundsOK() {
		t.Error("burst bounds violated after enable")
	}
}

// TestSetBurstLowerCeilingClamps: lowering the ceiling below the banked pot
// clamps the pot down; excess permission tokens are forfeited (no money impact).
func TestSetBurstLowerCeilingClamps(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	if err := bd.SetBurst(true, 1.0, 0, 0); err != nil { // ceiling = 10000
		t.Fatal(err)
	}
	bd.BurstSnapshot(1000) // bank to the full ceiling (idle whole window) => 10000
	if pot, _, _ := bd.BurstSnapshot(1000); pot != 10000 {
		t.Fatalf("precondition: pot = %d, want 10000", pot)
	}
	// Lower ceiling to 20% (=2000): pot must clamp to 2000.
	if err := bd.SetBurst(true, 0.2, 0, 1000); err != nil {
		t.Fatalf("SetBurst lower ceiling: %v", err)
	}
	pot, ceil, _ := bd.BurstSnapshot(1000)
	if ceil != 2000 || pot != 2000 {
		t.Errorf("after lowering: pot=%d ceil=%d, want 2000/2000", pot, ceil)
	}
	if !bd.BurstBoundsOK() {
		t.Error("burst bounds violated after clamp")
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("money conservation disturbed by a burst-config change")
	}
}

// TestSetBurstDisableWithLive: disabling burst while a job holds a reservation
// zeroes the bucket, and the job's MONEY still settles correctly (settle guards
// token refunds behind BurstEnabled).
func TestSetBurstDisableWithLive(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	bd.SetBurst(true, 1.0, 0, 0)
	bd.BurstSnapshot(300) // bank 3000
	bd.Submit("A", 50, 300)
	bd.Submit("B", 50, 300)
	bd.Start("A", 300)
	bd.Start("B", 300) // B reserves burst tokens; rLive=20
	if _, _, rl := bd.BurstSnapshot(300); rl != 20 {
		t.Fatalf("precondition rLive = %d, want 20", rl)
	}
	balBefore := bd.Balance()

	// Disable burst mid-flight.
	if err := bd.SetBurst(false, 0, 0, 300); err != nil {
		t.Fatalf("SetBurst disable: %v", err)
	}
	pot, _, rl := bd.BurstSnapshot(300)
	if pot != 0 || rl != 0 {
		t.Errorf("after disable pot=%d rLive=%d, want 0/0", pot, rl)
	}
	if bd.Balance() != balBefore {
		t.Errorf("disabling burst moved money: %d -> %d", balBefore, bd.Balance())
	}
	// The running jobs still settle their money correctly (no burst refund path).
	if err := bd.Complete("A", 20, 300); err != nil {
		t.Fatalf("settle A after disable: %v", err)
	}
	if err := bd.Complete("B", 20, 300); err != nil {
		t.Fatalf("settle B after disable: %v", err)
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken after disable+settle: sum=%d", sum)
	}
}

// TestSetBurstValidation: bad configs are rejected, ledger untouched.
func TestSetBurstValidation(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	cases := []struct {
		name    string
		enabled bool
		pct     float64
		cap     Units
	}{
		{"pct zero when enabling", true, 0, 0},
		{"pct over 1", true, 1.5, 0},
		{"negative cap", true, 0.5, -1},
		{"disable with pct", false, 0.5, 0},
		{"disable with cap", false, 0, 100},
	}
	for _, tc := range cases {
		if err := bd.SetBurst(tc.enabled, tc.pct, tc.cap, 0); !errors.Is(err, ErrBadState) {
			t.Errorf("%s: err = %v, want ErrBadState", tc.name, err)
		}
	}
	if bd.BurstEnabled {
		t.Error("a rejected SetBurst enabled burst")
	}
}

// TestSetBurstDurableRecovery: enable → submit → re-ceiling → submit → crash →
// recover reproduces state exactly, proving SetBurst is logged and replayed in
// order (a submit before the change replays under the old config, one after under
// the new). Burst bounds hold after recovery.
func TestSetBurstDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 10, 10000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.SetBurst(true, 1.0, 2000, 10); err != nil {
		t.Fatal(err)
	}
	bd.Submit("j1", 100, 20)
	bd.SetBurst(true, 0.3, 500, 30) // re-ceiling + new cap
	bd.Submit("j2", 100, 40)
	before := fingerprint(bd)
	_ = bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	r := rec.Report(40)
	if !r.BurstEnabled {
		t.Error("burst not enabled after recovery")
	}
	if r.BurstCeiling != 3000 { // 0.3 * 10000
		t.Errorf("recovered ceiling = %d, want 3000 (the second SetBurst)", r.BurstCeiling)
	}
	if !rec.BurstBoundsOK() {
		t.Error("burst bounds violated after recovery")
	}
	if ok, _ := rec.ConservationOK(); !ok {
		t.Error("conservation broken after recovery")
	}
}

// TestSetBurstDisableThenReEnable: a full off→on→off→on cycle keeps bounds/money
// sane (guards the enable-after-disable path where burstPot was zeroed).
func TestSetBurstReEnableAfterDisable(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	bd.SetBurst(true, 1.0, 0, 0)
	bd.BurstSnapshot(500) // bank 5000
	bd.SetBurst(false, 0, 0, 500)
	if pot, _, _ := bd.BurstSnapshot(500); pot != 0 {
		t.Fatal("pot not zeroed on disable")
	}
	// Re-enable: banking restarts from now (500), so still ~0 immediately.
	bd.SetBurst(true, 1.0, 0, 500)
	if pot, _, _ := bd.BurstSnapshot(500); pot != 0 {
		t.Errorf("re-enabled pot = %d, want 0 (banking restarts from now)", pot)
	}
	if pot, _, _ := bd.BurstSnapshot(600); pot != 1000 {
		t.Errorf("re-enabled pot after 100s = %d, want 1000", pot)
	}
	if !bd.BurstBoundsOK() {
		t.Error("bounds violated after re-enable")
	}
}
