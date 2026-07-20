package budget

import "testing"

// TestReportSnapshot checks the Report inspector reflects a mid-session state
// consistently: after one escrow and a partial settle, balance/reserved/consumed
// add up to B0 and the derived fields are right.
func TestReportSnapshot(t *testing.T) {
	bd := New(2, 1000, 0, 1000) // c=2, B0=1000, window [0,1000)

	r := bd.Report(0)
	if r.B != 1000 || r.B0 != 1000 || !r.ConservationOK {
		t.Fatalf("fresh report: %+v", r)
	}
	if tte := r.TimeToEmpty(); tte != 500 { // 1000 / 2
		t.Errorf("TimeToEmpty = %d, want 500", tte)
	}

	// Submit a 100s job: cost 200 escrowed.
	if err := bd.Submit("j1", 100, 1); err != nil {
		t.Fatal(err)
	}
	r = bd.Report(1)
	if r.B != 800 || r.Reserved != 200 || r.LiveEscrows != 1 {
		t.Errorf("after submit: B=%d reserved=%d live=%d", r.B, r.Reserved, r.LiveEscrows)
	}
	if !r.ConservationOK || r.ConservationSum != 1000 {
		t.Errorf("conservation: ok=%v sum=%d", r.ConservationOK, r.ConservationSum)
	}

	// Complete after 40s: bill 80, refund 120 -> B=920, consumed=80.
	if err := bd.Complete("j1", 40, 41); err != nil {
		t.Fatal(err)
	}
	r = bd.Report(41)
	if r.B != 920 || r.Consumed != 80 || r.Reserved != 0 || r.LiveEscrows != 0 {
		t.Errorf("after complete: B=%d consumed=%d reserved=%d live=%d", r.B, r.Consumed, r.Reserved, r.LiveEscrows)
	}
	if !r.ConservationOK {
		t.Errorf("conservation violated: sum=%d", r.ConservationSum)
	}
}

// TestReportTimeToEmptyNoBurn confirms a zero cost rate reports "never".
func TestReportTimeToEmptyNoBurn(t *testing.T) {
	bd := New(0, 500, 0, 1000)
	if tte := bd.Report(0).TimeToEmpty(); tte != -1 {
		t.Errorf("TimeToEmpty with c=0 = %d, want -1", tte)
	}
}

// TestReportLapsed reflects the lapsed lifecycle flag.
func TestReportLapsed(t *testing.T) {
	bd := New(1, 100, 0, 1000)
	if bd.Report(0).Lapsed {
		t.Error("fresh budget reported lapsed")
	}
	bd.Lapse()
	if !bd.Report(0).Lapsed {
		t.Error("lapsed budget not reported lapsed")
	}
}
