package budget

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// c=10/sec, B0=100000, window [0,10000) -> r0 = 10/sec. Array tasks each burn
// at C=10 == r0, so the FIRST running task is sustainable and each ADDITIONAL
// concurrent task bursts. This is the whole point: array concurrency bursts.
func freshArrBurst() *Budget {
	bd := New(10, 100000, 0, 10000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	return bd
}

func TestArrayBurstPerTaskDispatch(t *testing.T) {
	bd := freshArrBurst()
	bd.BurstSnapshot(1000)            // bank r0*1000 = 10000
	bd.SubmitArray("a", 4, 100, 1000) // 4 tasks, w=100; money escrowed whole

	// Task 0 dispatches: aggregate 0->10 (==r0), excess 0, no burst.
	if err := bd.StartTask("a", 0, 1000); err != nil {
		t.Fatal(err)
	}
	// Tasks 1,2,3 each add C=10 of burst: excess 10*100 = 1000 tokens each.
	for i := 1; i < 4; i++ {
		if err := bd.StartTask("a", i, 1000); err != nil {
			t.Fatalf("task %d dispatch: %v", i, err)
		}
	}
	pot, _, rl := bd.BurstSnapshot(1000)
	if pot != 7000 { // 10000 - 3*1000
		t.Fatalf("pot=%d want 7000 (3 bursting tasks * 1000)", pot)
	}
	if rl != 40 {
		t.Fatalf("rLive=%d want 40 (4 tasks * C=10)", rl)
	}
	mustConserveArr(t, bd)
	if !bd.BurstBoundsOK() {
		t.Fatal("burst bounds violated")
	}

	// Each bursting task completes early at 30/100s -> refund 70/100 of 1000 = 700.
	for i := 1; i < 4; i++ {
		if err := bd.CompleteTask("a", i, 30, 1000); err != nil {
			t.Fatal(err)
		}
	}
	bd.CompleteTask("a", 0, 30, 1000) // sustainable task, no burst refund
	pot2, _, rl2 := bd.BurstSnapshot(1000)
	if pot2 != 7000+3*700 { // 7000 + 2100 = 9100
		t.Fatalf("pot=%d want 9100 after refunds", pot2)
	}
	if rl2 != 0 {
		t.Fatalf("rLive=%d want 0", rl2)
	}
	if bd.LiveArrays() != 0 {
		t.Fatalf("array leaked: %d", bd.LiveArrays())
	}
	mustConserveArr(t, bd)
}

func TestArrayBurstDrawCapBlocks(t *testing.T) {
	bd := freshArrBurst()
	bd.BurstDrawCap = 500  // a task may reserve at most 500 tokens
	bd.BurstSnapshot(2000) // bank plenty
	bd.SubmitArray("a", 3, 100, 2000)
	bd.StartTask("a", 0, 2000) // sustainable
	// Task 1 excess 10*100 = 1000 > 500 cap -> dispatch blocked (task waits).
	if err := bd.StartTask("a", 1, 2000); !errors.Is(err, ErrBurstDrawCap) {
		t.Fatalf("want ErrBurstDrawCap got %v", err)
	}
	mustConserveArr(t, bd)
}

// Concurrent storm on arrays WITH burst: tasks within arrays race dispatch and
// settle, burst-blocked dispatches are tolerated (task waits). Asserts money
// conservation, per-escrow array invariant, AND burst bounds throughout.
func TestConcurrentArrayBurst(t *testing.T) {
	const (
		goroutines = 40
		nArrays    = 300
		c          = 6
		b0         = 30_000_000
		te         = 5_000_000 // r0 = 6M/5M = 1.2 -> fractional, exercises fixed-point
	)
	bd := New(c, b0, 0, te)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 0.5
	bd.BurstDrawCap = 100_000

	type arr struct {
		id string
		n  int
		w  Seconds
	}
	specs := make([]arr, nArrays)
	for i := range specs {
		specs[i] = arr{id: fmt.Sprintf("A%d", i), n: 1 + rand.Intn(20), w: Seconds(1 + rand.Intn(80))}
	}

	var negSeen, boundsViol int32
	var mu sync.Mutex
	type tk struct {
		id  string
		idx int
	}
	startable := []tk{} // submitted, not yet started
	running := []tk{}   // started, not yet settled
	submitted := map[string]bool{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			clk := Seconds(rng.Intn(int(te) / 2))
			for i := 0; i < nArrays*2; i++ {
				clk += Seconds(rng.Intn(60))
				if clk >= te {
					clk = Seconds(rng.Intn(int(te) / 2))
				}
				switch rng.Intn(3) {
				case 0: // submit an array
					s := specs[rng.Intn(nArrays)]
					if err := bd.SubmitArray(s.id, s.n, s.w, clk); err == nil {
						mu.Lock()
						if !submitted[s.id] {
							submitted[s.id] = true
							for k := 0; k < s.n; k++ {
								startable = append(startable, tk{s.id, k})
							}
						}
						mu.Unlock()
					}
				case 1: // try to dispatch a startable task
					mu.Lock()
					if len(startable) == 0 {
						mu.Unlock()
						break
					}
					j := rng.Intn(len(startable))
					t := startable[j]
					startable[j] = startable[len(startable)-1]
					startable = startable[:len(startable)-1]
					mu.Unlock()
					if err := bd.StartTask(t.id, t.idx, clk); err == nil {
						mu.Lock()
						running = append(running, t)
						mu.Unlock()
					} else {
						// burst-blocked or already settled: put back as startable
						mu.Lock()
						startable = append(startable, t)
						mu.Unlock()
					}
				case 2: // settle a running task
					mu.Lock()
					if len(running) == 0 {
						mu.Unlock()
						break
					}
					j := rng.Intn(len(running))
					t := running[j]
					running[j] = running[len(running)-1]
					running = running[:len(running)-1]
					mu.Unlock()
					_ = bd.CompleteTask(t.id, t.idx, Seconds(rng.Intn(5)), clk)
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

	// Drain: settle everything still live, by the model's own ground truth.
	for {
		bd.mu.Lock()
		var pickID string
		var pickIdx int
		found := false
		for id, ae := range bd.arrays {
			for idx, ts := range ae.tasks {
				if !ts.settled {
					pickID, pickIdx, found = id, idx, true
					break
				}
			}
			if found {
				break
			}
		}
		bd.mu.Unlock()
		if !found {
			break
		}
		_ = bd.StartTask(pickID, pickIdx, te-1) // ensure started so settle is clean
		_ = bd.CompleteTask(pickID, pickIdx, 1, te-1)
	}

	if negSeen == 1 {
		t.Fatal("overdraft in array+burst storm")
	}
	if boundsViol == 1 {
		t.Fatal("burst bounds violated mid-storm")
	}
	if ok, sum := bd.ConservationOKArrays(); !ok {
		t.Fatalf("array conservation violated: B0=%d got=%d", bd.B0, sum)
	}
	if !bd.BurstBoundsOK() {
		t.Fatal("burst bounds violated after drain")
	}
	if bd.rLive != 0 {
		t.Fatalf("rLive leaked: %d", bd.rLive)
	}
	if bd.LiveArrays() != 0 {
		t.Fatalf("arrays leaked: %d", bd.LiveArrays())
	}
}
