package budget

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestWALGroupCommitDurability confirms that with sync on, Flush makes all
// appended records durable (syncedOff catches up to writeOff) and the committer
// batches rather than syncing per-append.
func TestWALGroupCommitDurability(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(filepath.Join(dir, "wal.log"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < 100; i++ {
		if err := w.Append(Command{Kind: KindSubmit, JobID: "j", W: int64(i), Now: int64(i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Before flush, writeOff has advanced; syncedOff may lag (async committer).
	if w.Size() == 0 {
		t.Fatal("writeOff did not advance")
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// After an explicit flush, everything written is durable.
	if w.SyncedSize() != w.Size() {
		t.Errorf("after flush syncedOff=%d != writeOff=%d", w.SyncedSize(), w.Size())
	}
}

// TestWALGroupCommitConcurrent hammers Append from many goroutines (the daemon
// serializes appends under bd.mu in practice, but the WAL must be safe on its
// own). Run under -race. After Flush, all bytes are durable and recoverable.
func TestWALGroupCommitConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, err := OpenWAL(path, true)
	if err != nil {
		t.Fatal(err)
	}

	const n = 500
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.Append(Command{Kind: KindSubmit, JobID: "j", W: int64(i), Now: int64(i)})
		}(i)
	}
	wg.Wait()
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if w.SyncedSize() != w.Size() {
		t.Errorf("syncedOff=%d != writeOff=%d", w.SyncedSize(), w.Size())
	}
	syncedBytes := w.SyncedSize()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// All n records must replay back intact, and replay must consume the whole
	// durable file (end offset == synced size).
	count := 0
	endOff, err := replayWAL(path, 0, func(Command) error { count++; return nil })
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if count != n {
		t.Errorf("replayed %d records, want %d", count, n)
	}
	if endOff != syncedBytes {
		t.Errorf("replay end offset %d != durable bytes %d", endOff, syncedBytes)
	}
}

// TestWALGroupCommitRecovery confirms the group-commit WAL still recovers a
// budget correctly end to end: durable ops survive a crash (Close = flush+close),
// conservation holds.
func TestWALGroupCommitRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 10, 100000, 0, 10000, true) // sync ON
	if err != nil {
		t.Fatal(err)
	}
	bd.Submit("j1", 100, 10)
	bd.Start("j1", 20)
	bd.Submit("j2", 200, 30)
	bd.Complete("j1", 40, 60)
	before := fingerprint(bd)
	if err := bd.Close(); err != nil { // Close flushes: everything above is durable
		t.Fatalf("close: %v", err)
	}

	rec, err := OpenBudget(dir, true)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if ok, sum := rec.ConservationOK(); !ok {
		t.Errorf("recovered conservation broken: got=%d", sum)
	}
}

// TestWALTornTailStillDiscarded confirms group commit did not weaken the
// torn-tail discipline (invariant #4): an intact prefix recovers, a torn record
// at the tail is discarded. (Mirrors TestDurableTornTailDiscarded but on a
// sync-mode WAL.)
func TestWALTornTailStillDiscarded(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 10, 100000, 0, 10000, true)
	if err != nil {
		t.Fatal(err)
	}
	bd.Submit("j1", 100, 10)
	bd.Submit("j2", 100, 20)
	if err := bd.Close(); err != nil { // durable through j2
		t.Fatal(err)
	}

	// Append a torn record directly to the file.
	walPath := filepath.Join(dir, "wal.log")
	w, err := OpenWAL(walPath, false)
	if err != nil {
		t.Fatal(err)
	}
	// Write a valid header claiming more bytes than follow.
	if _, err := w.f.Write([]byte{0xE7, 0x03, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	rec, err := OpenBudget(dir, true)
	if err != nil {
		t.Fatalf("recovery should tolerate torn tail: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if rec.Live() != 2 {
		t.Fatalf("expected 2 escrows after torn-tail recovery, got %d", rec.Live())
	}
	if ok, _ := rec.ConservationOK(); !ok {
		t.Fatal("conservation broken after torn-tail recovery")
	}
}
