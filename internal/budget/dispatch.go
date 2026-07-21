package budget

// Burst dispatch query (issue #14, the site_factor plugin's tier-2 read).
//
// The site_factor priority hook asks, once per pending job every scheduling
// cycle, "would this job get the burst headroom to START right now, or must it
// hold at priority 0?" That is exactly the check bd.Start runs when it reserves
// burst tokens (budget.go), but it must be answered O(pending)/cycle WITHOUT
// taking bd.mu — taking the gate lock per pending job would contend the hot
// submit path (docs/SEAM_DESIGN.md §3, the reason readview.go exists).
//
// So MayDispatch reads the lock-free ReadView (one atomic load, internally
// consistent) and runs the SAME arithmetic Start does, through the shared pure
// helper burstDispatchVerdict. The locked headroom check (Simulate's
// burstHeadroomOK) routes through the same helper, so the lock-free answer can
// never drift from the real gate — the dispatch_test agreement matrix asserts it.
//
// It is advisory: between a true verdict and the actual Start, another job may
// consume the pot, so Start remains the authoritative atomic gate and may still
// return ErrBurstInsuff. Dispatch shapes priority; Start enforces.

// DispatchVerdict is the burst dispatch answer for one hypothetical job.
type DispatchVerdict struct {
	Dispatch bool   // may this job start now (true) or must it hold (false)?
	Reason   string // hold reason when !Dispatch ("" when Dispatch)
	Reserve  Units  // burst tokens the job would reserve (0 when under r0)
	Pot      Units  // projected burst pot at `now`
}

// burstProjection is the immutable set of fields the pure verdict math reads. It
// is populated from either the live budget (under lock) or a published ReadView
// (lock-free) — the helper does not know or care which.
//
// It deliberately carries NO lifecycle status: bd.Start does not gate on
// Active/Lapsed (the kernel lets live escrows start and settle on a lapsed
// budget), so for MayDispatch to agree with Start exactly, the dispatch verdict
// must not gate on status either. The money/window gate already happened at
// submit; dispatch is purely a burst-headroom question.
type burstProjection struct {
	burstEnabled    bool
	burstPot        Units
	rLive           Units
	fracAcc         Units
	lastTouch       Seconds
	b0              Units
	ts, te          Seconds
	burstCeilingPct float64
	burstDrawCap    Units
}

// burstDispatchVerdict is the single source of truth for the dispatch decision.
// It mirrors Start's burst reservation gate (budget.go) exactly: project the pot
// forward to `now`, compute the last-arriver reservation for rate c over walltime
// w, and apply the draw-cap and pot checks. It mutates nothing — all accrual is
// computed on locals. Both the lock-free MayDispatch and the locked
// burstHeadroomOK call through here so the two can never diverge.
func burstDispatchVerdict(p burstProjection, c, w, now Seconds) DispatchVerdict {
	// Burst disabled: Start never gates, so dispatch is always permitted.
	if !p.burstEnabled {
		return DispatchVerdict{Dispatch: true}
	}
	pot := projectBurstPot(p, now)
	resv := burstReserve(p.rLive, p.b0, p.ts, p.te, c, w)
	v := DispatchVerdict{Reserve: resv, Pot: pot}
	switch {
	case resv <= 0:
		v.Dispatch = true // under r0: no excess, no tokens needed
	case p.burstDrawCap > 0 && resv > p.burstDrawCap:
		v.Reason = "burst draw cap exceeded"
	case resv > pot:
		v.Reason = "insufficient burst headroom"
	default:
		v.Dispatch = true
	}
	return v
}

// projectBurstPot returns what the burst pot would be after accruing idle pace up
// to `now`, committing nothing — a pure copy of accrue's arithmetic (burst.go)
// operating on the projection's fields. Used only by the dispatch verdict.
func projectBurstPot(p burstProjection, now Seconds) Units {
	pot := p.burstPot
	window := p.te - p.ts
	if window <= 0 {
		return pot
	}
	dt := now - p.lastTouch
	if dt <= 0 {
		return pot
	}
	unusedNum := p.b0 - p.rLive*window // (r0 - rLive) * T
	if unusedNum > 0 {
		acc := p.fracAcc + unusedNum*dt
		pot += acc / window
		if ceil := Units(p.burstCeilingPct * float64(p.b0)); pot > ceil {
			pot = ceil
		}
	}
	return pot
}

// burstReserve is the last-arriver-pays reservation for a job at rate c over
// walltime w given aggregate live burn rLive: excess = (rLive+c) - max(rLive,
// floor(r0)); tokens = max(excess,0) * w. It is the free-standing form of
// burstReserveForRate (burst.go) so the pure verdict can compute without a *Budget.
func burstReserve(rLive, b0, ts, te, c, w Seconds) Units {
	window := te - ts
	if window <= 0 {
		return 0
	}
	r0f := b0 / window // floor of the sustainable rate
	base := rLive
	if base < r0f {
		base = r0f
	}
	excess := (rLive + c) - base
	if excess < 0 {
		excess = 0
	}
	return excess * w
}

// MayDispatch answers the burst dispatch query lock-free: would a job at cost
// rate c (units/second) for walltime w be allowed to start at time `now`, given
// the latest published state? It takes no lock — one atomic ReadView load feeds
// the shared verdict helper — so it is safe to call O(pending)/cycle from the
// site_factor path.
//
// c must be the resolved per-job rate (the daemon resolves node-type/TRES/flat
// before calling); unlike Simulate this does not fall back to bd.C, since C is
// not part of the lock-free view.
func (bd *Budget) MayDispatch(c, w, now Seconds) DispatchVerdict {
	v := bd.readView.Load()
	if v == nil {
		// Pre-New only (New seeds the view). Nothing published: permit — the
		// authoritative Start gate will still enforce.
		return DispatchVerdict{Dispatch: true}
	}
	return burstDispatchVerdict(burstProjection{
		burstEnabled:    v.BurstEnabled,
		burstPot:        v.BurstPot,
		rLive:           v.RLive,
		fracAcc:         v.FracAcc,
		lastTouch:       v.LastTouch,
		b0:              v.B0,
		ts:              v.TS,
		te:              v.TE,
		burstCeilingPct: v.BurstCeilingPct,
		burstDrawCap:    v.BurstDrawCap,
	}, c, w, now)
}
