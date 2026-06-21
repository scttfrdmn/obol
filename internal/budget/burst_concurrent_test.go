package budget

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentBurst storms the burst ledger: many goroutines racing submits
// (which reserve burst) and settles (which refund it), with independent logical
// clocks so accrual is exercised under contention and out-of-order time.
//
// Asserts, throughout and after: money conservation holds (burst is a separate
// ledger and must not corrupt it), burst bounds hold (0 <= burstPot <= ceiling,
// fracAcc in range), no overdraft, and rLive returns to 0 once drained.
func TestConcurrentBurst(t *testing.T) {
	const (
		goroutines = 48
		opsEach    = 2500
		c          = 7
		b0         = 60_000_000
		te         = 9_000_000 // r0 = b0/te is fractional (60M/9M = 6.66.../sec)
	)
	bd := New(c, b0, 0, te)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 0.5 // ceiling = 30M; must clamp under heavy idle
	bd.BurstDrawCap = 0      // unlimited per-job draw; bound comes from the bank

	var jobSeq int64
	var negSeen, boundsViol int32

	var liveMu sync.Mutex
	type liveJob struct{ w Seconds }
	live := map[string]liveJob{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			clk := Seconds(rng.Intn(int(te) / 2)) // independent, varied clock
			for i := 0; i < opsEach; i++ {
				clk += Seconds(rng.Intn(50)) // time generally advances, races across goroutines
				if clk >= te {
					clk = Seconds(rng.Intn(int(te) / 2))
				}
				switch rng.Intn(3) {
				case 0, 1: // submit
					w := Seconds(1 + rng.Intn(500))
					id := fmt.Sprintf("b%d", atomic.AddInt64(&jobSeq, 1))
					if err := bd.Submit(id, w, clk); err == nil {
						liveMu.Lock()
						live[id] = liveJob{w}
						liveMu.Unlock()
					}
				case 2: // settle a random live job
					liveMu.Lock()
					var pick string
					var lj liveJob
					for k, v := range live {
						pick, lj = k, v
						delete(live, k)
						break
					}
					liveMu.Unlock()
					if pick != "" {
						// Start may be burst-blocked; the job then settles unstarted.
						_ = bd.Start(pick, clk)
						_ = bd.Complete(pick, Seconds(rng.Intn(int(lj.w)+1)), clk)
					}
				}
				if bd.Balance() < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
				if !bd.BurstBoundsOK() {
					atomic.StoreInt32(&boundsViol, 1)
				}
			}
		}(int64(g)*1_000_003 + 1)
	}
	wg.Wait()

	// Drain remaining live jobs at a clock past everything.
	liveMu.Lock()
	rest := make([]string, 0, len(live))
	restW := make([]Seconds, 0, len(live))
	for id, lj := range live {
		rest = append(rest, id)
		restW = append(restW, lj.w)
	}
	liveMu.Unlock()
	for i, id := range rest {
		_ = bd.Start(id, te-1)
		_ = bd.Complete(id, restW[i], te-1)
	}

	if negSeen == 1 {
		t.Fatal("overdraft observed: money gate not atomic under burst")
	}
	if boundsViol == 1 {
		t.Fatal("burst bounds violated mid-storm")
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Fatalf("MONEY conservation violated under burst storm: B0=%d got=%d", bd.B0, sum)
	}
	if !bd.BurstBoundsOK() {
		t.Fatal("burst bounds violated after drain")
	}
	if bd.rLive != 0 {
		t.Fatalf("rLive leaked: %d (should be 0 after full drain)", bd.rLive)
	}
	if bd.Live() != 0 {
		t.Fatalf("escrows leaked: %d", bd.Live())
	}
}

// TestConcurrentBurstDrawCap reruns with a finite per-job draw cap so that gate
// branch is exercised under contention too.
func TestConcurrentBurstDrawCap(t *testing.T) {
	const goroutines, opsEach = 32, 2000
	bd := New(5, 40_000_000, 0, 8_000_000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	bd.BurstDrawCap = 50_000 // a single job may reserve at most 50k tokens

	var jobSeq int64
	var negSeen, boundsViol int32
	var liveMu sync.Mutex
	live := map[string]Seconds{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			clk := Seconds(rng.Intn(4_000_000))
			for i := 0; i < opsEach; i++ {
				clk += Seconds(rng.Intn(40))
				if clk >= 8_000_000 {
					clk = Seconds(rng.Intn(4_000_000))
				}
				if rng.Intn(2) == 0 {
					w := Seconds(1 + rng.Intn(300))
					id := fmt.Sprintf("d%d", atomic.AddInt64(&jobSeq, 1))
					if err := bd.Submit(id, w, clk); err == nil {
						liveMu.Lock()
						live[id] = w
						liveMu.Unlock()
					}
				} else {
					liveMu.Lock()
					var pick string
					var w Seconds
					for k, v := range live {
						pick, w = k, v
						delete(live, k)
						break
					}
					liveMu.Unlock()
					if pick != "" {
						_ = bd.Start(pick, clk)
						_ = bd.Complete(pick, Seconds(rng.Intn(int(w)+1)), clk)
					}
				}
				if bd.Balance() < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
				if !bd.BurstBoundsOK() {
					atomic.StoreInt32(&boundsViol, 1)
				}
			}
		}(int64(g)*7_654_321 + 3)
	}
	wg.Wait()

	liveMu.Lock()
	rest := make(map[string]Seconds, len(live))
	for k, v := range live {
		rest[k] = v
	}
	liveMu.Unlock()
	for id, w := range rest {
		_ = bd.Start(id, 7_999_999)
		_ = bd.Complete(id, w, 7_999_999)
	}

	if negSeen == 1 {
		t.Fatal("overdraft under burst draw-cap storm")
	}
	if boundsViol == 1 {
		t.Fatal("burst bounds violated mid-storm (draw cap)")
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Fatalf("money conservation violated: B0=%d got=%d", bd.B0, sum)
	}
}
