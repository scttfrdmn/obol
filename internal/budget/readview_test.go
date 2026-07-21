package budget

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestReadSnapshotReflectsMutations confirms the lock-free read view tracks the
// write path: after each mutation, ReadSnapshot returns the new aggregates.
func TestReadSnapshotReflectsMutations(t *testing.T) {
	bd := New(10, 100000, 0, 10000)

	if v := bd.ReadSnapshot(); v.B != 100000 {
		t.Fatalf("initial B = %d, want 100000", v.B)
	}
	// Submit debits B by c*w = 10*100 = 1000.
	if err := bd.Submit("j1", 100, 10); err != nil {
		t.Fatal(err)
	}
	if v := bd.ReadSnapshot(); v.B != 99000 {
		t.Errorf("after submit B = %d, want 99000", v.B)
	}
	// Complete refunds the unused tail: runtime 40 -> used 400 -> refund 600.
	if err := bd.Complete("j1", 40, 60); err != nil {
		t.Fatal(err)
	}
	if v := bd.ReadSnapshot(); v.B != 99600 {
		t.Errorf("after complete B = %d, want 99600", v.B)
	}
}

// TestReadSnapshotBurstFields confirms burstPot and rLive are published.
func TestReadSnapshotBurstFields(t *testing.T) {
	bd := New(10, 100000, 0, 10000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0
	bd.Submit("j1", 100, 10)
	bd.Start("j1", 20) // dispatch -> rLive += C
	if v := bd.ReadSnapshot(); v.RLive != 10 {
		t.Errorf("after start rLive = %d, want 10", v.RLive)
	}
	bd.Complete("j1", 40, 60) // settle -> rLive back to 0
	if v := bd.ReadSnapshot(); v.RLive != 0 {
		t.Errorf("after complete rLive = %d, want 0", v.RLive)
	}
}

// TestReadSnapshotConcurrentNoContention hammers ReadSnapshot from many readers
// while gate writes run, under -race. It asserts (a) no read ever observes a
// torn/impossible triple, and (b) reads never take the write lock (they can't —
// ReadSnapshot has no access to bd.mu). This models the site_factor tier-2 load.
func TestReadSnapshotConcurrentNoContention(t *testing.T) {
	bd := New(1, 1_000_000, 0, 1_000_000)

	var stop atomic.Bool
	var readCount atomic.Int64
	var readers, writers sync.WaitGroup

	// Readers spin on the lock-free snapshot, asserting each published view is
	// internally consistent (no field ever negative — a torn read would show it).
	for r := 0; r < 8; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				v := bd.ReadSnapshot()
				if v.B < 0 || v.RLive < 0 || v.BurstPot < 0 {
					t.Errorf("torn/negative read view: %+v", v)
					return
				}
				readCount.Add(1)
			}
		}()
	}

	// Writers drive the gate write path with submit/complete cycles.
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(base int) {
			defer writers.Done()
			for i := 0; i < 2000; i++ {
				id := itoa(base) + "-" + itoa(i)
				if bd.Submit(id, 10, Seconds(i)) == nil {
					_ = bd.Complete(id, 5, Seconds(i+1))
				}
			}
		}(w)
	}

	writers.Wait()   // let all gate writes complete
	stop.Store(true) // then release the readers
	readers.Wait()

	if readCount.Load() == 0 {
		t.Error("readers observed nothing")
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken after concurrent load: %d", sum)
	}
	// Final view must match the settled balance (all jobs completed).
	if v := bd.ReadSnapshot(); v.B != bd.Balance() {
		t.Errorf("final read view B=%d != Balance()=%d", v.B, bd.Balance())
	}
}

// itoa is a tiny int->string to avoid importing strconv just for unique ids.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
