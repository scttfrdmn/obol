package budget

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

func mustConserveArr(t *testing.T, bd *Budget) {
	t.Helper()
	ok, sum := bd.ConservationOKArrays()
	if !ok {
		t.Fatalf("array conservation violated: B0=%d got=%d (B=%d res=%d cons=%d wo=%d)",
			bd.B0, sum, bd.B, bd.ReservedTotal, bd.Consumed, bd.WriteOff)
	}
}

// c=10, B0=100000 -> 10000 funded seconds. Period [0,100000).
func freshArr() *Budget { return New(10, 100000, 0, 100000) }

func TestArraySubmitDebitsWhole(t *testing.T) {
	bd := freshArr()
	// N=10 tasks, w=50 -> cost = 10*50*10 = 5000.
	if err := bd.SubmitArray("a1", 10, 50, 0); err != nil {
		t.Fatal(err)
	}
	if bd.Balance() != 95000 {
		t.Fatalf("balance=%d want 95000", bd.Balance())
	}
	mustConserveArr(t, bd)
}

func TestArrayAllOrNothing(t *testing.T) {
	bd := New(10, 4999, 0, 100000) // can't afford 5000
	if err := bd.SubmitArray("a1", 10, 50, 0); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient got %v", err)
	}
	if bd.Balance() != 4999 || bd.LiveArrays() != 0 {
		t.Fatalf("rejected array mutated state: B=%d arrays=%d", bd.Balance(), bd.LiveArrays())
	}
	mustConserveArr(t, bd)
}

func TestArrayPerTaskSettlement(t *testing.T) {
	bd := freshArr()
	bd.SubmitArray("a1", 4, 100, 0) // cost 10*100*4 = 4000; slice = 1000 each
	// Task 0 completes early at 30s -> used 300, refund 700.
	bd.StartTask("a1", 0, 0)
	bd.CompleteTask("a1", 0, 30, 0)
	if bd.Balance() != 96000+700 { // 96000 after submit, +700 refund
		t.Fatalf("balance=%d want 96700", bd.Balance())
	}
	// Task 1 times out -> used 1000, refund 0.
	bd.StartTask("a1", 1, 0)
	bd.TimeoutTask("a1", 1, 0)
	// Task 2 cancelled pre-run -> full slice refund 1000.
	bd.CancelTask("a1", 2, 0, 0)
	// Task 3 infra-fails on-prem at 50s -> writeoff 500, refund 500.
	bd.BillInfraFailures = false
	bd.StartTask("a1", 3, 0)
	bd.InfraFailTask("a1", 3, 50, 0)

	if bd.LiveArrays() != 0 {
		t.Fatalf("array should be closed, %d live", bd.LiveArrays())
	}
	if bd.WriteOff != 500 {
		t.Fatalf("writeoff=%d want 500", bd.WriteOff)
	}
	mustConserveArr(t, bd)
}

func TestArrayDoubleSettleRejected(t *testing.T) {
	bd := freshArr()
	bd.SubmitArray("a1", 3, 100, 0)
	bd.StartTask("a1", 0, 0)
	if err := bd.CompleteTask("a1", 0, 10, 0); err != nil {
		t.Fatal(err)
	}
	// Second settle of the same task must be rejected, not silently corrupt.
	if err := bd.CompleteTask("a1", 0, 10, 0); !errors.Is(err, ErrBadState) {
		t.Fatalf("double-settle want ErrBadState got %v", err)
	}
	mustConserveArr(t, bd)
}

func TestArrayPartialThenLapse(t *testing.T) {
	bd := freshArr()
	bd.SubmitArray("big", 100, 100, 0) // cost 10*100*100 = 100000 == B0, dumps pot
	if bd.Balance() != 0 {
		t.Fatalf("balance=%d want 0 (whole pot escrowed)", bd.Balance())
	}
	// Settle 60 tasks early (runtime 0 -> full refund each, 1000/task).
	for i := 0; i < 60; i++ {
		bd.CancelTask("big", i, 0, 0)
	}
	if bd.Balance() != 60000 {
		t.Fatalf("balance=%d want 60000", bd.Balance())
	}
	// Period ends with 40 tasks still live and prepaid.
	bd.Lapse()
	mustConserveArr(t, bd) // 40 live tasks: reserved must == 40*1000
	// Remaining tasks complete after lapse; refunds land in lapsed pot.
	for i := 60; i < 100; i++ {
		bd.StartTask("big", i, 0)
		bd.CompleteTask("big", i, 25, 0) // used 250, refund 750 each
	}
	if bd.LiveArrays() != 0 {
		t.Fatalf("arrays leaked: %d", bd.LiveArrays())
	}
	mustConserveArr(t, bd)
}

// The new contention surface: many goroutines racing tasks WITHIN arrays, plus
// arrays racing each other for the pot. Asserts global books AND every live
// array's per-escrow equality hold throughout, with no overdraft.
func TestConcurrentArrays(t *testing.T) {
	const (
		goroutines = 48
		nArrays    = 400
		c          = 7
		b0         = 80_000_000
	)
	bd := New(c, b0, 0, 1<<62)

	type arr struct {
		id string
		n  int
		w  Seconds
	}
	// Pre-generate array specs; submit may fail on solvency, that's fine.
	specs := make([]arr, nArrays)
	for i := range specs {
		specs[i] = arr{id: fmt.Sprintf("A%d", i), n: 1 + rand.Intn(40), w: Seconds(1 + rand.Intn(60))}
	}

	var negSeen int32
	// Track which (array,task) pairs are live and settleable.
	var mu sync.Mutex
	type tk struct {
		id  string
		idx int
	}
	liveTasks := []tk{}
	submitted := map[string]bool{}

	var wg sync.WaitGroup

	// Submitters.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < nArrays; i++ {
				s := specs[rng.Intn(nArrays)]
				if err := bd.SubmitArray(s.id, s.n, s.w, 0); err == nil {
					mu.Lock()
					if !submitted[s.id] {
						submitted[s.id] = true
						for k := 0; k < s.n; k++ {
							liveTasks = append(liveTasks, tk{s.id, k})
						}
					}
					mu.Unlock()
				}
				if bd.Balance() < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
			}
		}(int64(g)*1_000_003 + 1)
	}

	// Settlers.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < nArrays*3; i++ {
				mu.Lock()
				if len(liveTasks) == 0 {
					mu.Unlock()
					continue
				}
				j := rng.Intn(len(liveTasks))
				t := liveTasks[j]
				liveTasks[j] = liveTasks[len(liveTasks)-1]
				liveTasks = liveTasks[:len(liveTasks)-1]
				mu.Unlock()

				bd.StartTask(t.id, t.idx, 0)
				switch rng.Intn(3) {
				case 0:
					_ = bd.CompleteTask(t.id, t.idx, Seconds(rng.Intn(5)), 0)
				case 1:
					_ = bd.TimeoutTask(t.id, t.idx, 0)
				case 2:
					_ = bd.InfraFailTask(t.id, t.idx, Seconds(rng.Intn(5)), 0)
				}
				if bd.Balance() < 0 {
					atomic.StoreInt32(&negSeen, 1)
				}
			}
		}(int64(g)*7_654_321 + 5)
	}

	wg.Wait()

	// Drain any tasks still live, using the MODEL's own state as ground truth
	// rather than the harness's racy shadow list. (The shadow list can miss
	// tasks of arrays submitted after a settler sampled it — that's a harness
	// artifact, not a model bug. We settle whatever the budget says is live.)
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
		bd.StartTask(pickID, pickIdx, 0)
		_ = bd.CompleteTask(pickID, pickIdx, 1, 0)
	}

	if negSeen == 1 {
		t.Fatal("overdraft observed in array storm")
	}
	if ok, sum := bd.ConservationOKArrays(); !ok {
		t.Fatalf("array conservation violated after storm: B0=%d got=%d", bd.B0, sum)
	}
	if bd.LiveArrays() != 0 {
		t.Fatalf("arrays leaked: %d", bd.LiveArrays())
	}
}
