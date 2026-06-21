package budget

import (
	"errors"
	"testing"
)

func mustConserve(t *testing.T, bd *Budget) {
	t.Helper()
	ok, sum := bd.ConservationOK()
	if !ok {
		t.Fatalf("conservation violated: B0=%d got=%d (B=%d res=%d cons=%d wo=%d)",
			bd.B0, sum, bd.B, bd.ReservedTotal, bd.Consumed, bd.WriteOff)
	}
}

// c=10 units/sec, B0=10000 -> funds 1000 sec total. Period [0,1000).
func fresh() *Budget { return New(10, 10000, 0, 1000) }

func TestSubmitDebits(t *testing.T) {
	bd := fresh()
	if err := bd.Submit("j1", 100, 0); err != nil { // cost 1000
		t.Fatal(err)
	}
	if bd.Balance() != 9000 {
		t.Fatalf("balance=%d want 9000", bd.Balance())
	}
	mustConserve(t, bd)
}

func TestSolvencyGate(t *testing.T) {
	bd := fresh()
	// cost = 10 * 1001 = 10010 > 10000 -> reject, no mutation.
	if err := bd.Submit("big", 1001, 0); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient got %v", err)
	}
	if bd.Balance() != 10000 || bd.Live() != 0 {
		t.Fatalf("rejected submit mutated state: B=%d live=%d", bd.Balance(), bd.Live())
	}
	mustConserve(t, bd)
}

func TestCompleteRefundsTail(t *testing.T) {
	bd := fresh()
	bd.Submit("j1", 100, 0) // reserve 1000 for 100s
	bd.Start("j1", 0)
	bd.Complete("j1", 40, 0) // used 400, refund 600
	if bd.Balance() != 9600 {
		t.Fatalf("balance=%d want 9600", bd.Balance())
	}
	if bd.Consumed != 400 {
		t.Fatalf("consumed=%d want 400", bd.Consumed)
	}
	mustConserve(t, bd)
}

func TestTimeoutNoRefund(t *testing.T) {
	bd := fresh()
	bd.Submit("j1", 100, 0)
	bd.Start("j1", 0)
	bd.Timeout("j1", 0) // used full 100s -> 1000, refund 0
	if bd.Balance() != 9000 {
		t.Fatalf("balance=%d want 9000", bd.Balance())
	}
	mustConserve(t, bd)
}

func TestCancelPreRunFullRefund(t *testing.T) {
	bd := fresh()
	bd.Submit("j1", 100, 0)
	bd.Cancel("j1", 0, 0) // never started
	if bd.Balance() != 10000 {
		t.Fatalf("balance=%d want 10000 (full unwind)", bd.Balance())
	}
	if bd.Consumed != 0 {
		t.Fatalf("consumed=%d want 0", bd.Consumed)
	}
	mustConserve(t, bd)
}

func TestCancelRunningBillsElapsed(t *testing.T) {
	bd := fresh()
	bd.Submit("j1", 100, 0)
	bd.Start("j1", 0)
	bd.Cancel("j1", 30, 0) // billed 300, refund 700
	if bd.Balance() != 9700 || bd.Consumed != 300 {
		t.Fatalf("B=%d consumed=%d", bd.Balance(), bd.Consumed)
	}
	mustConserve(t, bd)
}

func TestInfraFailOnPremWritesOff(t *testing.T) {
	bd := fresh()
	bd.BillInfraFailures = false // on-prem: infra loss is free to user
	bd.Submit("j1", 100, 0)
	bd.Start("j1", 0)
	bd.InfraFail("j1", 50, 0) // 500 written off, refund 500
	if bd.Balance() != 9500 {
		t.Fatalf("balance=%d want 9500", bd.Balance())
	}
	if bd.WriteOff != 500 || bd.Consumed != 0 {
		t.Fatalf("writeoff=%d consumed=%d want 500/0", bd.WriteOff, bd.Consumed)
	}
	mustConserve(t, bd)
}

func TestInfraFailCloudBills(t *testing.T) {
	bd := fresh()
	bd.BillInfraFailures = true // cloud: user pays
	bd.Submit("j1", 100, 0)
	bd.Start("j1", 0)
	bd.InfraFail("j1", 50, 0) // 500 billed, no writeoff
	if bd.Balance() != 9500 || bd.Consumed != 500 || bd.WriteOff != 0 {
		t.Fatalf("B=%d consumed=%d wo=%d", bd.Balance(), bd.Consumed, bd.WriteOff)
	}
	mustConserve(t, bd)
}

func TestLapseClosesSubmit(t *testing.T) {
	bd := fresh()
	bd.Lapse()
	if err := bd.Submit("j1", 10, 0); !errors.Is(err, ErrLapsed) {
		t.Fatalf("want ErrLapsed got %v", err)
	}
	mustConserve(t, bd)
}

func TestLiveJobSurvivesLapseAndRefundsToLapsedPot(t *testing.T) {
	bd := fresh()
	bd.Submit("j1", 100, 0)
	bd.Start("j1", 0)
	bd.Lapse()               // period ends while job runs
	bd.Complete("j1", 20, 0) // prepaid; refund 800 lands in lapsed pot
	if bd.Balance() != 9800 {
		t.Fatalf("balance=%d want 9800", bd.Balance())
	}
	mustConserve(t, bd)
}

func TestBurstCeilingClamps(t *testing.T) {
	bd := fresh()
	bd.K = 1.0 // no banking: cannot reserve above sustainable rate
	// At now=0, r = B/(te-now) = 10000/1000 = 10 units/sec.
	// reserved_total starts 0. A job's rate is cost/?? — note: the gate checks
	// reserved_total+cost <= k*r, i.e. total reserved units <= k * (units/sec).
	// With k=1, ceiling is 10 units. cost = c*w = 10*1 = 10 for w=1 -> just fits.
	if err := bd.Submit("ok", 1, 0); err != nil {
		t.Fatalf("w=1 should fit ceiling: %v", err)
	}
	// Now reserved_total=10. Another even tiny job exceeds 10. Reject.
	if err := bd.Submit("no", 1, 0); !errors.Is(err, ErrRateExceeded) {
		t.Fatalf("want ErrRateExceeded got %v", err)
	}
	mustConserve(t, bd)
}

func TestBurstInfiniteByDefault(t *testing.T) {
	bd := fresh() // K=0 -> infinite
	// Dump the whole pot in one job: w=1000, cost=10000 == B0. Allowed.
	if err := bd.Submit("dump", 1000, 0); err != nil {
		t.Fatalf("dump should be allowed with K=0: %v", err)
	}
	if bd.Balance() != 0 {
		t.Fatalf("balance=%d want 0", bd.Balance())
	}
	mustConserve(t, bd)
}
