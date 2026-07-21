package main

import (
	"testing"
	"time"
)

// singleBudgetCfg builds a config for the single-budget back-compat path.
func singleBudgetCfg(dir string, create bool, rate, b0 int64, window time.Duration) config {
	return config{dir: dir, create: create, rate: rate, b0: b0, window: window}
}

// TestBuildRegistryWindowAnchored is a regression test: a freshly created budget
// must have its window anchored at the current clock (epoch seconds), matching
// the `now` the server feeds transitions. Otherwise every gate sees now >= TE
// and rejects as lapsed.
func TestBuildRegistryWindowAnchored(t *testing.T) {
	dir := t.TempDir()
	reg, _, err := buildRegistry(singleBudgetCfg(dir, true, 1, 5000, 30*24*time.Hour))
	if err != nil {
		t.Fatalf("buildRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	bd, err := reg.Resolve("default")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	now := time.Now().Unix()
	if err := bd.Submit("j1", 100, now); err != nil {
		t.Fatalf("submit at wall-clock now failed: %v (window not anchored to clock)", err)
	}
	if bal := bd.Balance(); bal != 4900 {
		t.Errorf("balance after submit = %d, want 4900", bal)
	}
}

// TestBuildRegistryNoCreateFails confirms building without -create over an empty
// state dir is an error, not a silent empty budget.
func TestBuildRegistryNoCreateFails(t *testing.T) {
	if _, _, err := buildRegistry(singleBudgetCfg(t.TempDir(), false, 1, 5000, time.Hour)); err == nil {
		t.Fatal("expected error building registry without -create over empty dir")
	}
}

// TestBuildRegistryReopens confirms a created budget reopens (recovers) on a
// second build without -create.
func TestBuildRegistryReopens(t *testing.T) {
	dir := t.TempDir()
	reg, _, err := buildRegistry(singleBudgetCfg(dir, true, 2, 1000, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Close()

	reg2, _, err := buildRegistry(singleBudgetCfg(dir, false, 0, 0, 0)) // no -create; must recover
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer func() { _ = reg2.Close() }()
	bd, err := reg2.Resolve("default")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if b0 := bd.Report(time.Now().Unix()).B0; b0 != 1000 {
		t.Errorf("recovered B0 = %d, want 1000", b0)
	}
}
