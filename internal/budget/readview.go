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
//
// It carries everything MayDispatch (the lock-free burst dispatch query, #14)
// needs to project the burst pot to `now` and compute a per-job reservation
// WITHOUT taking bd.mu: the mutable bucket state (BurstPot, RLive, FracAcc,
// LastTouch) plus the config the projection reads (B0, window, ceiling, cap).
// All fields come from one locked publish, so the view is internally consistent
// — a set of independent atomics could not guarantee that (fracAcc/lastTouch are
// mutated together on every accrue and must be read together).
type ReadView struct {
	B        Units // current balance
	BurstPot Units // banked burst tokens (0 when burst disabled)
	RLive    Units // aggregate live burn rate

	// Burst-projection inputs (for MayDispatch). Config fields are effectively
	// immutable at runtime this round; the bucket fields are the ones that make a
	// naive multi-load tear, which is why they ride the single atomic publish.
	FracAcc         Units   // sub-unit accrual remainder (numerator over window T)
	LastTouch       Seconds // accrual clock
	B0              Units   // original allocation (for r0 and ceiling)
	TS, TE          Seconds // window bounds
	BurstEnabled    bool
	BurstCeilingPct float64
	BurstDrawCap    Units
}

// publishLocked snapshots the current aggregates into the atomic read view. The
// caller must hold bd.mu (every mutating transition does). It is cheap — one
// small allocation and an atomic store — and keeps the lock-free read path
// current without the reader ever taking the lock.
func (bd *Budget) publishLocked() {
	bd.readView.Store(&ReadView{
		B: bd.B, BurstPot: bd.burstPot, RLive: bd.rLive,
		FracAcc: bd.fracAcc, LastTouch: bd.lastTouch, B0: bd.B0,
		TS: bd.TS, TE: bd.TE,
		BurstEnabled: bd.BurstEnabled, BurstCeilingPct: bd.BurstCeilingPct, BurstDrawCap: bd.BurstDrawCap,
	})
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
