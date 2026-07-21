package budget

// Tier-2 read path (docs/SEAM_DESIGN.md §3/§13.6). The site_factor priority hook
// runs O(pending jobs) per scheduling cycle and only needs to read a few live
// aggregates — balance, burst pot, live burn rate. Taking the gate's write mutex
// for each of those reads would contend the hot submit path. Instead, every
// mutation publishes an immutable ReadView under the lock it already holds, and
// readers load it lock-free through an atomic pointer. Reads therefore never
// touch bd.mu and never block a gate write.
//
// The published triple is internally consistent (all three fields come from the
// same locked moment), which a set of three independent atomics could not
// guarantee.

// ReadView is an immutable, point-in-time snapshot of the live aggregates a
// priority/burst reader needs. It is published by the write path and read
// lock-free.
type ReadView struct {
	B        Units // current balance
	BurstPot Units // banked burst tokens (0 when burst disabled)
	RLive    Units // aggregate live burn rate
}

// publishLocked snapshots the current aggregates into the atomic read view. The
// caller must hold bd.mu (every mutating transition does). It is cheap — one
// small allocation and an atomic store — and keeps the lock-free read path
// current without the reader ever taking the lock.
func (bd *Budget) publishLocked() {
	bd.readView.Store(&ReadView{B: bd.B, BurstPot: bd.burstPot, RLive: bd.rLive})
}

// ReadSnapshot returns the latest published aggregates WITHOUT taking bd.mu, so
// it never contends the gate write path. The values are as of the most recent
// completed mutation. Returns the zero view before the first publish (a
// freshly-created budget publishes once in New, so this is only the pre-New case).
func (bd *Budget) ReadSnapshot() ReadView {
	if v := bd.readView.Load(); v != nil {
		return *v
	}
	return ReadView{}
}
