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

// SweepOrphans settles every live escrow/array-task whose job ID is not present
// in liveIDs. For arrays, liveIDs membership is checked per array ID (an array
// is "live" if its array ID is in the set). Returns the count swept. `now` is
// used for burst accounting on settlement.
func (bd *Budget) SweepOrphans(liveIDs map[string]bool, policy OrphanPolicy, now Seconds) int {
	// Collect orphan IDs first (can't mutate maps while ranging via settle).
	bd.mu.Lock()
	var orphanJobs []struct {
		id string
		w  Seconds
	}
	for id, e := range bd.escrows {
		if !liveIDs[id] {
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
			if !ts.settled {
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
