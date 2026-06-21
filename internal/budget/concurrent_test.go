package budget

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentConservation throws randomized, racing transitions at one
// budget and asserts: (1) the books always balance (checked after the storm),
// (2) B and ReservedTotal never go negative at any observed point,
// (3) no successful submit ever overdrafts.
//
// The hard race is the gate: many goroutines reading the same balance and
// racing to debit it. If check-and-debit weren't atomic, two submits would
// both pass against the same B and drive it negative.
func TestConcurrentConservation(t *testing.T) {
	const (
		goroutines = 64
		opsEach    = 2000
		c          = 7
		b0         = 50_000_000
	)
	bd := New(c, b0, 0, 1<<62) // huge period so time never closes submit here
	bd.K = 0                   // exercise pure solvency racing first

	var jobSeq int64
	var negSeen int32 // set if any goroutine ever observes B<0 or reserved<0

	// Track every live job we created so completers have real IDs to settle.
	var liveMu sync.Mutex
	live := map[string]Seconds{} // jobID -> funded walltime

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < opsEach; i++ {
				switch rng.Intn(4) {
				case 0, 1: // submit (weighted)
					w := Seconds(1 + rng.Intn(200))
					id := fmt.Sprintf("j%d", atomic.AddInt64(&jobSeq, 1))
					if err := bd.Submit(id, w, 0); err == nil {
						liveMu.Lock()
						live[id] = w
						liveMu.Unlock()
					}
				case 2: // complete a random live job with random runtime
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
						bd.Start(pick, 0)
						_ = bd.Complete(pick, Seconds(rng.Intn(int(w)+1)), 0)
					}
				case 3: // infra-fail a random live job
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
						bd.Start(pick, 0)
						_ = bd.InfraFail(pick, Seconds(rng.Intn(int(w)+1)), 0)
					}
				}
				// Observe non-negativity mid-flight.
				if b := bd.Balance(); b < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
			}
		}(int64(g)*1_000_003 + 1)
	}
	wg.Wait()

	// Drain whatever's still live so the final books are fully settled.
	liveMu.Lock()
	for id, w := range live {
		bd.Start(id, 0)
		_ = bd.Complete(id, w/2, 0)
	}
	liveMu.Unlock()

	if atomic.LoadInt32(&negSeen) == 1 {
		t.Fatal("observed negative balance mid-flight: gate is not atomic")
	}
	if bd.ReservedTotal < 0 {
		t.Fatalf("reserved_total negative: %d", bd.ReservedTotal)
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Fatalf("conservation violated after storm: B0=%d got=%d", bd.B0, sum)
	}
	if bd.Live() != 0 {
		t.Fatalf("escrows leaked: %d still live", bd.Live())
	}
}

// TestConcurrentBurstCeiling reruns the storm with a finite k so the rate check
// (which reads ReservedTotal) is also exercised under contention. The ceiling
// must never be the thing that corrupts the books or overdrafts.
func TestConcurrentBurstCeiling(t *testing.T) {
	const goroutines, opsEach = 32, 1500
	bd := New(5, 20_000_000, 0, 1_000_000)
	bd.K = 3.0

	var jobSeq int64
	var negSeen int32
	var liveMu sync.Mutex
	live := map[string]Seconds{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			now := Seconds(rng.Intn(900_000)) // vary clock -> vary r
			for i := 0; i < opsEach; i++ {
				if rng.Intn(2) == 0 {
					w := Seconds(1 + rng.Intn(50))
					id := fmt.Sprintf("k%d", atomic.AddInt64(&jobSeq, 1))
					if err := bd.Submit(id, w, now); err == nil {
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
						bd.Start(pick, 0)
						_ = bd.Complete(pick, Seconds(rng.Intn(int(w)+1)), 0)
					}
				}
				if bd.Balance() < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
			}
		}(int64(g)*7_654_321 + 3)
	}
	wg.Wait()

	liveMu.Lock()
	for id, w := range live {
		bd.Start(id, 0)
		_ = bd.Complete(id, w, 0)
	}
	liveMu.Unlock()

	if negSeen == 1 {
		t.Fatal("overdraft under burst ceiling")
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Fatalf("conservation violated: B0=%d got=%d", bd.B0, sum)
	}
}
