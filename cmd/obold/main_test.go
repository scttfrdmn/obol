package main

import (
	"testing"
	"time"
)

// TestOpenOrCreateWindowAnchored is a regression test: a freshly created budget
// must have its window anchored at the current clock (epoch seconds), matching
// the `now` the server feeds transitions. Otherwise every gate sees now >= TE
// and rejects as lapsed.
func TestOpenOrCreateWindowAnchored(t *testing.T) {
	dir := t.TempDir()
	bd, err := openOrCreate(dir, false, true, 1, 5000, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("openOrCreate: %v", err)
	}
	defer func() { _ = bd.Close() }()

	// A submit stamped with the real clock must succeed — proving now is inside
	// [TS, TE). Before the fix the window was [0, ~2.6M) and this returned ErrLapsed.
	now := time.Now().Unix()
	if err := bd.Submit("j1", 100, now); err != nil {
		t.Fatalf("submit at wall-clock now failed: %v (window not anchored to clock)", err)
	}
	if bal := bd.Balance(); bal != 4900 {
		t.Errorf("balance after submit = %d, want 4900", bal)
	}
}

// TestOpenOrCreateNoCreateFails confirms opening a nonexistent budget without
// -create is an error, not a silent empty budget.
func TestOpenOrCreateNoCreateFails(t *testing.T) {
	if _, err := openOrCreate(t.TempDir(), false, false, 1, 5000, time.Hour); err == nil {
		t.Fatal("expected error opening nonexistent budget without -create")
	}
}

// TestOpenOrCreateReopens confirms a created budget reopens (recovers) on a
// second call without -create.
func TestOpenOrCreateReopens(t *testing.T) {
	dir := t.TempDir()
	bd, err := openOrCreate(dir, false, true, 2, 1000, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = bd.Close()

	bd2, err := openOrCreate(dir, false, false, 0, 0, 0) // no -create; must recover
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer func() { _ = bd2.Close() }()
	if b0 := bd2.Report(time.Now().Unix()).B0; b0 != 1000 {
		t.Errorf("recovered B0 = %d, want 1000", b0)
	}
}
