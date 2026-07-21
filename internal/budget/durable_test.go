package budget

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// stateFingerprint captures the externally-observable state for equality checks
// across a crash/recover boundary.
func fingerprint(bd *Budget) string {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	return fmt.Sprintf("B=%d res=%d cons=%d wo=%d pot=%d frac=%d rl=%d lt=%d stat=%d esc=%d arr=%d",
		bd.B, bd.ReservedTotal, bd.Consumed, bd.WriteOff,
		bd.burstPot, bd.fracAcc, bd.rLive, bd.lastTouch, bd.status,
		len(bd.escrows), len(bd.arrays))
}

func TestDurableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 10, 100000, 0, 10000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	// Burst flags aren't logged commands, so they must be set before NewDurable's
	// snapshot OR re-set on recovery. Re-snapshot to capture them.
	if err := bd.Snapshot(); err != nil {
		t.Fatal(err)
	}

	// A spread of operations.
	bd.Submit("j1", 100, 50)
	bd.Start("j1", 60)
	bd.Submit("j2", 200, 70)
	bd.Complete("j1", 40, 120)
	bd.SubmitArray("arr", 5, 80, 130)
	bd.StartTask("arr", 0, 140)
	bd.StartTask("arr", 1, 150)
	bd.CompleteTask("arr", 0, 30, 200)
	bd.Submit("j3", 50, 210)
	bd.Cancel("j2", 0, 220)

	before := fingerprint(bd)
	okBefore, _ := bd.ConservationOKArrays()
	if !okBefore {
		t.Fatal("pre-crash conservation broken")
	}
	bd.Close() // flush; simulate process exit (we keep the files)

	// CRASH: drop the in-memory budget entirely, recover from disk.
	recovered, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	after := fingerprint(recovered)
	if before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if ok, sum := recovered.ConservationOKArrays(); !ok {
		t.Fatalf("recovered conservation broken: got=%d", sum)
	}
	// Recovery must be live: a new op continues from exact state.
	if err := recovered.Complete("j3", 10, 230); err != nil {
		t.Fatalf("post-recovery op failed: %v", err)
	}
}

func TestDurableTornTailDiscarded(t *testing.T) {
	dir := t.TempDir()
	bd, _ := NewDurable(dir, 10, 100000, 0, 10000, false)
	bd.Submit("j1", 100, 10)
	bd.Submit("j2", 100, 20)
	bd.Close()

	// Append a torn record: a valid header claiming N bytes, but fewer follow.
	walPath := filepath.Join(dir, "wal.log")
	f, _ := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o644)
	// length=999, junk crc, then only a few bytes (less than 999) -> torn.
	f.Write([]byte{0xE7, 0x03, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF})
	f.Write([]byte("partial"))
	f.Close()

	recovered, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery should tolerate torn tail: %v", err)
	}
	// j1 and j2 must be present (their records were intact); the torn record is
	// discarded as never-committed.
	if recovered.Live() != 2 {
		t.Fatalf("expected 2 escrows after torn-tail recovery, got %d", recovered.Live())
	}
	if ok, _ := recovered.ConservationOK(); !ok {
		t.Fatal("conservation broken after torn-tail recovery")
	}
}

func TestDurableSnapshotThenReplay(t *testing.T) {
	dir := t.TempDir()
	bd, _ := NewDurable(dir, 10, 100000, 0, 10000, false)
	bd.Submit("j1", 100, 10)
	bd.Complete("j1", 50, 60)             // j1 fully settled before snapshot
	if err := bd.Snapshot(); err != nil { // snapshot covers j1's lifecycle
		t.Fatal(err)
	}
	// Post-snapshot ops live only in the WAL tail past the snapshot offset.
	bd.Submit("j2", 200, 70)
	bd.Start("j2", 80)
	bd.Submit("j3", 50, 90)
	before := fingerprint(bd)
	bd.Close()

	recovered, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if after := fingerprint(recovered); before != after {
		t.Fatalf("snapshot+replay diverged:\n before=%s\n after =%s", before, after)
	}
	// j1 must NOT be double-applied (it's in the snapshot, its WAL records are
	// before the snapshot offset and skipped). Money books prove it.
	if ok, sum := recovered.ConservationOK(); !ok {
		t.Fatalf("double-apply or loss: conservation got=%d", sum)
	}
}

func TestOrphanJanitor(t *testing.T) {
	dir := t.TempDir()
	bd, _ := NewDurable(dir, 10, 100000, 0, 10000, false)
	bd.Submit("alive", 100, 10)
	bd.Submit("orphan1", 100, 10)
	bd.SubmitArray("orphanArr", 3, 50, 10)
	bd.StartTask("orphanArr", 0, 20)

	balBefore := bd.Balance()
	// Slurm reports only "alive" still exists.
	live := map[string]bool{"alive": true}
	swept := bd.SweepOrphans(live, OrphanConsumeFull, 300)
	// orphan1 (1) + 3 array tasks (3) = 4 swept.
	if swept != 4 {
		t.Fatalf("swept=%d want 4", swept)
	}
	if ok, sum := bd.ConservationOKArrays(); !ok {
		t.Fatalf("conservation broken after sweep: got=%d", sum)
	}
	if bd.LiveArrays() != 0 {
		t.Fatalf("orphan array not cleared: %d", bd.LiveArrays())
	}
	// "alive" untouched, its money still escrowed.
	if _, ok := bd.escrows["alive"]; !ok {
		t.Fatal("janitor swept a live job")
	}
	// ConsumeFull => no refund, so balance unchanged (orphans billed full walltime).
	if bd.Balance() != balBefore {
		t.Fatalf("ConsumeFull refunded: %d -> %d", balBefore, bd.Balance())
	}
}

// TestConfigDurableAcrossRecovery locks in the decision on issue #8: config
// (cost rate, window, and every policy flag) is set at creation, captured in the
// snapshot, and survives a snapshot + WAL-replay recovery unchanged. Config is
// immutable post-creation today; if mutation is ever added it must arrive as a
// logged command (never snapshot-only), or WAL replay would lose its ordering
// against the command stream and break the pure-(state,command,now) invariant.
func TestConfigDurableAcrossRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 7, 500000, 100, 200100, false) // c=7, non-zero TS/TE
	if err != nil {
		t.Fatal(err)
	}
	// Set every policy flag to a non-default value, then snapshot so the initial
	// snapshot captures them (they are config, not logged commands).
	bd.BillInfraFailures = true
	bd.AllowRequeue = true
	bd.K = 2.5
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 0.75
	bd.BurstDrawCap = 1234
	if err := bd.Snapshot(); err != nil {
		t.Fatal(err)
	}

	// Do some work so recovery also replays a WAL tail past the config snapshot.
	bd.Submit("j1", 100, 150)
	bd.Start("j1", 160)
	bd.Complete("j1", 40, 200)
	bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	// Every config field must match exactly.
	checks := []struct {
		name      string
		got, want any
	}{
		{"C", rec.C, Units(7)},
		{"TS", rec.TS, Seconds(100)},
		{"TE", rec.TE, Seconds(200100)},
		{"B0", rec.B0, Units(500000)},
		{"BillInfraFailures", rec.BillInfraFailures, true},
		{"AllowRequeue", rec.AllowRequeue, true},
		{"K", rec.K, 2.5},
		{"BurstEnabled", rec.BurstEnabled, true},
		{"BurstCeilingPct", rec.BurstCeilingPct, 0.75},
		{"BurstDrawCap", rec.BurstDrawCap, Units(1234)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("config %s not durable: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if ok, sum := rec.ConservationOK(); !ok {
		t.Errorf("recovered conservation broken: got=%d", sum)
	}
}
