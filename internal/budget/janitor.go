package budget

// Orphan reconciliation. After a crash, an escrow or array task may be stuck:
// its job vanished from Slurm without firing a completion event, so its money
// is reserved forever. The janitor sweeps escrows/tasks whose job is NOT in the
// live set Slurm reports, settling them so conservation is restored.
//
// This is the ONLY reconciler in the system, and it is a janitor, not a gate:
// it never blocks admission, it only cleans stuck reservations.
//
// assumedRuntime decides how an orphan is billed. The common cause is a lost
// completion event for a job that DID finish, so the safe-for-budget default is
// to treat it as having consumed its full walltime (no refund); callers wanting
// generous treatment can pass a smaller runtime.

// OrphanPolicy selects how the janitor bills an orphaned escrow when it sweeps.
type OrphanPolicy int

const (
	// OrphanConsumeFull assumes the orphan ran its full walltime (no refund).
	// Safe against budget overspend; correct when the completion event was lost.
	OrphanConsumeFull OrphanPolicy = iota
	// OrphanRefundFull assumes the orphan never ran (full refund). Generous to
	// the user; use only when you know orphans didn't consume.
	OrphanRefundFull
)

// SweepUnbound reconciles the submit→start orphan window (docs/SEAM_DESIGN.md
// §4/§13.2). Between the GATE (escrow minted against a correlation token) and the
// job's first appearance to the site_factor/prolog path (token↔jobid bound, the
// escrow marked Started), the daemon knows the job only as an UNBOUND token. If
// the daemon crashes and recovers in that window it holds an escrow that never
// started and that the jobid-based SweepOrphans can never match — its money is
// reserved forever.
//
// SweepUnbound settles any escrow that (a) never started and (b) was submitted at
// least ttl seconds before now, with a FULL REFUND: an unstarted job provably
// consumed nothing, so Cancel(id, 0, now) returns the whole reservation. The ttl
// distinguishes a job legitimately waiting to dispatch (recent, kept) from a
// presumed-dead unbound token (stale, swept). Returns the count swept.
//
// A recent unbound escrow (age < ttl) is left alone — it may simply be a pending
// job the scheduler hasn't started yet. Only staleness makes it an orphan.
func (bd *Budget) SweepUnbound(ttl Seconds, now Seconds) int {
	// Collect stale, never-started IDs first (can't mutate maps while settling).
	bd.mu.Lock()
	var ids []string
	for id, e := range bd.escrows {
		if !e.Started && now-e.Submitted >= ttl {
			ids = append(ids, id)
		}
	}
	type taskRef struct {
		arrayID string
		idx     int
	}
	var tasks []taskRef
	for aid, ae := range bd.arrays {
		if now-ae.Submitted < ttl {
			continue
		}
		// An array is unbound only if NONE of its tasks ever started. If any task
		// dispatched, the array is live work — leave it to the normal path.
		anyStarted := false
		for _, ts := range ae.tasks {
			if ts.started {
				anyStarted = true
				break
			}
		}
		if anyStarted {
			continue
		}
		for idx, ts := range ae.tasks {
			if !ts.settled {
				tasks = append(tasks, taskRef{aid, idx})
			}
		}
	}
	bd.mu.Unlock()

	swept := 0
	for _, id := range ids {
		if bd.Cancel(id, 0, now) == nil { // never started => full refund
			swept++
		}
	}
	for _, tr := range tasks {
		if bd.CancelTask(tr.arrayID, tr.idx, 0, now) == nil {
			swept++
		}
	}
	return swept
}

// SweepOrphans settles every STARTED escrow/array-task whose job ID is not
// present in liveIDs — the lost-completion class (a job that ran, then vanished
// from Slurm without a completion event) and the started-orphan-after-crash class
// (a bound escrow whose daemon routing was lost on restart). For arrays, liveIDs
// membership is checked per array ID. Returns the count swept; `now` is used for
// burst accounting on settlement.
//
// It deliberately skips NEVER-STARTED (unbound) work: such an escrow has no bound
// job id, so it can never be in a jobid-derived liveIDs set and would be wrongly
// swept here — that submit→start orphan class belongs to SweepUnbound (#15), which
// ages it out by TTL. Requiring Started keeps the two janitors from racing: this
// one owns bound work, SweepUnbound owns unbound work.
func (bd *Budget) SweepOrphans(liveIDs map[string]bool, policy OrphanPolicy, now Seconds) int {
	// Collect orphan IDs first (can't mutate maps while ranging via settle).
	bd.mu.Lock()
	var orphanJobs []struct {
		id string
		w  Seconds
	}
	for id, e := range bd.escrows {
		if e.Started && !liveIDs[id] {
			orphanJobs = append(orphanJobs, struct {
				id string
				w  Seconds
			}{id, e.W})
		}
	}
	type taskRef struct {
		arrayID string
		idx     int
		w       Seconds
	}
	var orphanTasks []taskRef
	for aid, ae := range bd.arrays {
		if liveIDs[aid] {
			continue
		}
		for idx, ts := range ae.tasks {
			if ts.started && !ts.settled {
				orphanTasks = append(orphanTasks, taskRef{aid, idx, ae.W})
			}
		}
	}
	bd.mu.Unlock()

	swept := 0
	for _, o := range orphanJobs {
		var err error
		switch policy {
		case OrphanRefundFull:
			err = bd.Cancel(o.id, 0, now) // unstarted-style full refund
		default:
			err = bd.Timeout(o.id, now) // consumed full walltime, no refund
		}
		if err == nil {
			swept++
		}
	}
	for _, o := range orphanTasks {
		var err error
		switch policy {
		case OrphanRefundFull:
			err = bd.CancelTask(o.arrayID, o.idx, 0, now)
		default:
			err = bd.TimeoutTask(o.arrayID, o.idx, now)
		}
		if err == nil {
			swept++
		}
	}
	return swept
}
