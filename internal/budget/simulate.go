package budget

// Simulation is the read-only verdict for a hypothetical submission (the `obol
// simulate`/`estimate` verb, issue #21): would this job be admitted right now,
// what would it cost, and how much runway remains. It mirrors the gate's checks
// (solvency, window/status, the optional rate ceiling) and the dispatch-time
// burst headroom check — WITHOUT debiting or banking anything.
type Simulation struct {
	Cost   Units   // c * w at the effective rate
	Admit  bool    // would the gate admit this job now?
	Reason string  // deny reason when !Admit ("" when admitted)
	Runway Seconds // time-to-empty at the current balance and rate (B/C), -1 if C<=0
}

// Simulate reports whether a job at rate c (c<=0 means the budget's flat rate)
// for walltime w would be admitted at time now, plus its cost and the budget's
// runway — changing NO state. It is the honest dry run the gate would perform:
// the money solvency + window + rate-ceiling checks (as in SubmitAt), and the
// burst dispatch headroom check (as in Start) evaluated against a *projected*
// accrual so the real bucket is untouched.
func (bd *Budget) Simulate(c, w Seconds, now Seconds) Simulation {
	bd.mu.Lock()
	defer bd.mu.Unlock()

	if c <= 0 {
		c = bd.C
	}
	cost := c * w
	sim := Simulation{Cost: cost, Runway: -1}
	if bd.C > 0 {
		sim.Runway = bd.B / bd.C
	}

	switch {
	case bd.status != Active:
		sim.Reason = "budget lapsed"
	case now < bd.TS || now >= bd.TE:
		sim.Reason = "outside budget window"
	case cost > bd.B:
		sim.Reason = "insufficient budget"
	case !bd.rateOK(cost, now):
		sim.Reason = "burst rate ceiling exceeded"
	case bd.BurstEnabled && !bd.burstHeadroomOK(c, w, now):
		sim.Reason = "insufficient burst headroom"
	default:
		sim.Admit = true
	}
	return sim
}

// burstHeadroomOK reports whether a job at rate c for walltime w could reserve
// the burst tokens the dispatch gate (Start) would demand, WITHOUT mutating the
// bucket. It projects the accrual to `now` (mirroring accrue's arithmetic) and
// compares the required reservation against the projected pot and the draw cap.
func (bd *Budget) burstHeadroomOK(c, w Seconds, now Seconds) bool {
	resv := bd.burstReserveForRate(c, w)
	if resv <= 0 {
		return true // no excess over r0 => no tokens needed
	}
	if bd.BurstDrawCap > 0 && resv > bd.BurstDrawCap {
		return false
	}
	return resv <= bd.projectedBurstPot(now)
}

// projectedBurstPot returns what burstPot would be after accruing idle pace up to
// `now`, without committing (a pure copy of accrue's math). Used by the read-only
// simulation so it never changes the real bucket.
func (bd *Budget) projectedBurstPot(now Seconds) Units {
	pot := bd.burstPot
	dt := now - bd.lastTouch
	if dt <= 0 {
		return pot
	}
	T := bd.window()
	unusedNum := bd.B0 - bd.rLive*T // (r0 - rLive) * T
	if unusedNum > 0 {
		acc := bd.fracAcc + unusedNum*dt
		pot += acc / T
		if ceil := bd.burstCeiling(); pot > ceil {
			pot = ceil
		}
	}
	return pot
}
