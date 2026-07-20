package budget

// Report is a read-only, point-in-time snapshot of a budget for inspection
// (the obol `show` verb). It is additive: it introduces no transition and
// touches no money or burst state — it only reads, under one lock, what several
// separate inspectors would otherwise read across several lock acquisitions.
// Reading it all under a single lock makes `show` a consistent snapshot.
type Report struct {
	// Config
	C  Units   // cost per second
	B0 Units   // original allocation
	TS Seconds // period start
	TE Seconds // period end

	// Money ledger (conservation: B0 == B + Reserved + Consumed + WriteOff)
	B        Units // current balance
	Reserved Units // Σ reserved over live escrows/arrays
	Consumed Units // billed to the user
	WriteOff Units // absorbed by the system

	// Live work
	LiveEscrows int // unsettled 1:1 escrows
	LiveArrays  int // arrays with unsettled tasks

	// Burst ledger (0 when burst disabled)
	BurstEnabled bool
	BurstPot     Units
	BurstCeiling Units
	RLive        Units

	// Derived
	Lapsed          bool
	ConservationOK  bool  // B0 == B + Reserved + Consumed + WriteOff
	ConservationSum Units // the computed sum, for reporting a violation
}

// Report returns a consistent snapshot of the budget under a single lock. It
// accrues burst to `now` first (matching BurstSnapshot) so the reported burst
// pot reflects idle banking up to the moment of the call.
func (bd *Budget) Report(now Seconds) Report {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if bd.BurstEnabled {
		bd.accrue(now)
	}
	sum := bd.B + bd.ReservedTotal + bd.Consumed + bd.WriteOff
	s := Report{
		C: bd.C, B0: bd.B0, TS: bd.TS, TE: bd.TE,
		B: bd.B, Reserved: bd.ReservedTotal, Consumed: bd.Consumed, WriteOff: bd.WriteOff,
		LiveEscrows:     len(bd.escrows),
		LiveArrays:      len(bd.arrays),
		BurstEnabled:    bd.BurstEnabled,
		RLive:           bd.rLive,
		Lapsed:          bd.status == Lapsed,
		ConservationOK:  sum == bd.B0,
		ConservationSum: sum,
	}
	if bd.BurstEnabled {
		s.BurstPot = bd.burstPot
		s.BurstCeiling = bd.burstCeiling()
	}
	return s
}

// TimeToEmpty estimates seconds until the balance is exhausted at the budget's
// cost rate C, from the current balance. Returns -1 when C <= 0 (no burn, never
// empties). This is a projection for `show`, not a guarantee.
func (s Report) TimeToEmpty() Seconds {
	if s.C <= 0 {
		return -1
	}
	return s.B / s.C
}
