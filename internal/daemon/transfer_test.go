package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// conserves asserts a budget's single invariant holds.
func conserves(t *testing.T, bd *budget.Budget, label string) {
	t.Helper()
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("%s: conservation broken (sum=%d)", label, sum)
	}
}

// TestTransferMovesAndConserves: a transfer moves the amount, and BOTH budgets
// conserve individually + the cross-budget total is invariant.
func TestTransferMovesAndConserves(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	smith, _ := reg.Resolve("lab_smith") // 100000
	jones, _ := reg.Resolve("lab_jones") // 50000
	total := smith.Balance() + jones.Balance()

	moved, err := reg.Transfer("lab_smith", "lab_jones", 30000, false)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if moved != 30000 {
		t.Errorf("moved = %d, want 30000", moved)
	}
	if smith.Balance() != 70000 {
		t.Errorf("smith = %d, want 70000", smith.Balance())
	}
	if jones.Balance() != 80000 {
		t.Errorf("jones = %d, want 80000", jones.Balance())
	}
	if got := smith.Balance() + jones.Balance(); got != total {
		t.Errorf("cross-budget total = %d, want %d (money created/destroyed)", got, total)
	}
	conserves(t, smith, "smith")
	conserves(t, jones, "jones")

	// The journal record is cleaned up after a clean transfer.
	if entries, _ := os.ReadDir(filepath.Join(dir, transferDir)); len(entries) != 0 {
		t.Errorf("journal not cleaned: %d records remain", len(entries))
	}
}

// TestTransferAllSweepsBalance: --all moves the source's entire available B.
func TestTransferAllSweepsBalance(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	smith, _ := reg.Resolve("lab_smith")
	// Reserve some of smith so available < allocation: only available B moves.
	if err := smith.Submit("live", 10000, testNow()); err != nil {
		t.Fatal(err)
	}
	avail := smith.Balance() // 90000
	moved, err := reg.Transfer("lab_smith", "lab_jones", 0, true)
	if err != nil {
		t.Fatalf("Transfer --all: %v", err)
	}
	if moved != avail {
		t.Errorf("moved = %d, want %d (available balance)", moved, avail)
	}
	if smith.Balance() != 0 {
		t.Errorf("smith available = %d, want 0", smith.Balance())
	}
	conserves(t, smith, "smith") // reserved 10000 is still accounted
}

// TestTransferRejects: same-account, insufficient, and unknown accounts are
// rejected with no money moved and no lingering journal.
func TestTransferRejects(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	smith, _ := reg.Resolve("lab_smith")

	if _, err := reg.Transfer("lab_smith", "lab_smith", 100, false); err == nil {
		t.Error("same-account transfer should be rejected")
	}
	if _, err := reg.Transfer("lab_smith", "lab_jones", 999999, false); err == nil {
		t.Error("over-balance transfer should be rejected")
	}
	if _, err := reg.Transfer("ghost", "lab_jones", 100, false); err == nil {
		t.Error("unknown source should be rejected")
	}
	if _, err := reg.Transfer("lab_smith", "ghost", 100, false); err == nil {
		t.Error("unknown destination should be rejected")
	}
	if smith.Balance() != 100000 {
		t.Errorf("smith balance changed by a rejected transfer: %d", smith.Balance())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, transferDir)); len(entries) != 0 {
		t.Errorf("rejected transfers left %d journal records", len(entries))
	}
}

// TestTransferRecoversWithdrawOnly simulates a crash AFTER the withdraw leg
// committed but BEFORE the deposit: a journal record + a tagged withdraw in the
// source WAL, nothing in the destination. Recovery must complete the deposit so
// money is neither lost nor duplicated.
func TestTransferRecoversWithdrawOnly(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	smith, _ := reg.Resolve("lab_smith")
	jones, _ := reg.Resolve("lab_jones")
	total := smith.Balance() + jones.Balance()

	// Simulate the interrupted state by hand: commit only the withdraw leg with a
	// known xfer id, and write the journal record — exactly what an on-disk state
	// looks like after a crash between the two legs.
	const xid = "xfer:deadbeef"
	if err := smith.WithdrawXfer(20000, xid, testNow()); err != nil {
		t.Fatal(err)
	}
	if err := writeTransferRecord(dir, transferRecord{
		XferID: xid, From: "lab_smith", To: "lab_jones", Amount: 20000, Now: testNow(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Close() // flush WALs

	// Reopen: recovery should complete the deposit leg.
	reg2, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatalf("reopen with recovery: %v", err)
	}
	defer func() { _ = reg2.Close() }()
	smith2, _ := reg2.Resolve("lab_smith")
	jones2, _ := reg2.Resolve("lab_jones")

	if smith2.Balance() != 80000 {
		t.Errorf("recovered smith = %d, want 80000", smith2.Balance())
	}
	if jones2.Balance() != 70000 {
		t.Errorf("recovered jones = %d, want 70000 (deposit not completed)", jones2.Balance())
	}
	if got := smith2.Balance() + jones2.Balance(); got != total {
		t.Errorf("post-recovery total = %d, want %d", got, total)
	}
	conserves(t, smith2, "smith")
	conserves(t, jones2, "jones")
	if entries, _ := os.ReadDir(filepath.Join(dir, transferDir)); len(entries) != 0 {
		t.Errorf("recovery did not clear the journal: %d remain", len(entries))
	}
	// Idempotent: the deposit must not double-apply if recovery runs again.
	_ = reg2.Close()
	reg3, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg3.Close() }()
	jones3, _ := reg3.Resolve("lab_jones")
	if jones3.Balance() != 70000 {
		t.Errorf("second recovery double-applied deposit: jones = %d, want 70000", jones3.Balance())
	}
}

// TestTransferRecoversNeitherLeg simulates a crash after the journal was written
// but BEFORE either leg committed. No money moved, so recovery must abort the
// transfer (not re-run it) and clear the record.
func TestTransferRecoversNeitherLeg(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTransferRecord(dir, transferRecord{
		XferID: "xfer:cafe", From: "lab_smith", To: "lab_jones", Amount: 12345, Now: testNow(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Close()

	reg2, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatalf("reopen with recovery: %v", err)
	}
	defer func() { _ = reg2.Close() }()
	smith2, _ := reg2.Resolve("lab_smith")
	jones2, _ := reg2.Resolve("lab_jones")
	if smith2.Balance() != 100000 || jones2.Balance() != 50000 {
		t.Errorf("aborted transfer moved money: smith=%d jones=%d, want 100000/50000",
			smith2.Balance(), jones2.Balance())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, transferDir)); len(entries) != 0 {
		t.Errorf("recovery did not clear the aborted journal: %d remain", len(entries))
	}
}

// TestTransferRecoveryRejectsCorruptOrdering: a journal record with a committed
// DEPOSIT but no withdraw is an impossible ordering (deposit never precedes
// withdraw) → recovery surfaces it loudly rather than silently minting money.
func TestTransferRecoveryRejectsCorruptOrdering(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	jones, _ := reg.Resolve("lab_jones")
	const xid = "xfer:bad"
	if err := jones.TopUpXfer(5000, xid, testNow()); err != nil { // deposit with no withdraw
		t.Fatal(err)
	}
	if err := writeTransferRecord(dir, transferRecord{
		XferID: xid, From: "lab_smith", To: "lab_jones", Amount: 5000, Now: testNow(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Close()

	_, err = NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err == nil {
		t.Fatal("expected recovery to reject a deposit-without-withdraw as corruption")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error = %v, want a corruption error", err)
	}
}

// TestHandleTransferAdminGated: transfer is a mutating verb — non-admins are
// rejected before any money moves; an admin succeeds.
func TestHandleTransferAdminGated(t *testing.T) {
	srv := newAdminServer(t) // lab_smith (1000), lab_jones (500); admins alice(10)/ops(11)
	smith, _ := srv.reg.Resolve("lab_smith")

	// mallory (13, not admin) rejected, no movement.
	resp := srv.handleTransfer(&wire.TransferRequest{From: "lab_smith", To: "lab_jones", Amount: 200}, adminPeer(13))
	if resp.TransferResp == nil || resp.TransferResp.OK {
		t.Fatalf("non-admin transfer should be rejected: %+v", resp.TransferResp)
	}
	if smith.Balance() != 1000 {
		t.Errorf("balance moved on a rejected transfer: %d", smith.Balance())
	}
	// alice (10, admin) succeeds.
	resp = srv.handleTransfer(&wire.TransferRequest{From: "lab_smith", To: "lab_jones", Amount: 200}, adminPeer(10))
	if resp.TransferResp == nil || !resp.TransferResp.OK {
		t.Fatalf("admin transfer rejected: %+v", resp.TransferResp)
	}
	if resp.TransferResp.Moved != 200 || resp.TransferResp.FromBalance != 800 || resp.TransferResp.ToBalance != 700 {
		t.Errorf("unexpected transfer result: %+v", resp.TransferResp)
	}
}

// TestTransferConcurrentConserves hammers a pair of accounts with round-trip
// transfers from many goroutines. Each transfer is atomic; the cross-budget total
// must be invariant throughout (-race). Round-tripping keeps the net deterministic.
func TestTransferConcurrentConserves(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	smith, _ := reg.Resolve("lab_smith")
	jones, _ := reg.Resolve("lab_jones")
	total := smith.Balance() + jones.Balance()

	const workers, iters = 6, 40
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// A small round trip nets zero but exercises both directions under -race.
				if _, err := reg.Transfer("lab_smith", "lab_jones", 100, false); err != nil {
					continue // a transient insufficient is fine; we assert conservation, not success
				}
				_, _ = reg.Transfer("lab_jones", "lab_smith", 100, false)
			}
		}()
	}
	wg.Wait()

	if got := smith.Balance() + jones.Balance(); got != total {
		t.Errorf("cross-budget total drifted under concurrency: %d, want %d", got, total)
	}
	conserves(t, smith, "smith")
	conserves(t, jones, "jones")
}
