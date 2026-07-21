package budget

import "testing"

// TestReadLogReflectsTransitions confirms ReadLog returns every committed
// transition in commit order, with the fields that matter (amounts, rates).
func TestReadLogReflectsTransitions(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	// A representative spread.
	bd.TopUp(5000, 5)
	bd.SubmitAt("j1", 3, 100, 10)
	bd.Start("j1", 20)
	bd.Complete("j1", 40, 60)
	bd.SubmitArray("arr", 4, 50, 70)
	bd.Lapse()
	bd.Close()

	entries, err := ReadLog(dir)
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}

	wantKinds := []string{"topup", "submit", "start", "settle:complete", "submit-array", "lapse"}
	if len(entries) != len(wantKinds) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(wantKinds), entries)
	}
	for i, want := range wantKinds {
		if entries[i].Kind != want {
			t.Errorf("entry %d kind = %q, want %q", i, entries[i].Kind, want)
		}
	}
	// Spot-check payloads.
	if entries[0].Amount != 5000 {
		t.Errorf("topup amount = %d, want 5000", entries[0].Amount)
	}
	if entries[1].JobID != "j1" || entries[1].Rate != 3 || entries[1].W != 100 {
		t.Errorf("submit entry = %+v, want job=j1 rate=3 w=100", entries[1])
	}
	if entries[3].Runtime != 40 {
		t.Errorf("complete runtime = %d, want 40", entries[3].Runtime)
	}
	if entries[4].ArrayID != "arr" || entries[4].N != 4 {
		t.Errorf("submit-array entry = %+v, want array=arr n=4", entries[4])
	}
}

// TestReadLogEmpty confirms a fresh budget's log is empty (no transitions yet),
// not an error.
func TestReadLogEmpty(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 1000, 0, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.Close()
	entries, err := ReadLog(dir)
	if err != nil {
		t.Fatalf("ReadLog on fresh budget: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("fresh budget log = %d entries, want 0", len(entries))
	}
}
