package budget

import "path/filepath"

// Audit log. The WAL is already an append-only record of every committed
// transition (never truncated — see Snapshot's note), so it IS the audit trail.
// ReadLog surfaces it read-only for the `obol log` verb, without touching or
// replaying live budget state.

// LogEntry is one rendered WAL record: the transition kind and the fields that
// matter for that kind. It is a flattened, presentation-friendly view of a
// Command — the CLI formats it; the daemon ships a slice of these.
type LogEntry struct {
	Kind    string  // human transition name (e.g. "submit", "settle:complete", "topup")
	JobID   string  // 1:1 escrow / array id (whichever applies)
	ArrayID string  // array id for array transitions
	Idx     int     // task index for array-task transitions
	N       int     // task count (submit-array)
	Rate    Units   // per-job cost rate (submit)
	W       Seconds // funded walltime (submit)
	Runtime Seconds // billed runtime (complete/*-task)
	Elapsed Seconds // elapsed (cancel/infrafail)
	Amount  Units   // top-up amount
	TS      Seconds // window start (set-window)
	TE      Seconds // window end (set-window)
	Now     Seconds // logical clock at the transition
}

// commandKindName maps a WAL command kind to a readable label.
func commandKindName(k string) string {
	switch k {
	case KindSubmit:
		return "submit"
	case KindStart:
		return "start"
	case KindComplete:
		return "settle:complete"
	case KindTimeout:
		return "settle:timeout"
	case KindCancel:
		return "settle:cancel"
	case KindInfraFail:
		return "settle:infrafail"
	case KindSubmitArray:
		return "submit-array"
	case KindStartTask:
		return "start-task"
	case KindCompleteTask:
		return "settle-task:complete"
	case KindTimeoutTask:
		return "settle-task:timeout"
	case KindCancelTask:
		return "settle-task:cancel"
	case KindInfraFailTask:
		return "settle-task:infrafail"
	case KindLapse:
		return "lapse"
	case KindTopUp:
		return "topup"
	case KindReprice:
		return "reprice"
	case KindSetRate:
		return "set-rate"
	case KindSetWindow:
		return "set-window"
	default:
		return k
	}
}

// Log returns this budget's audit log by reading its own WAL directory. It is
// read-only and takes no lock — the WAL is append-only, so a concurrent live
// append simply isn't seen by this read. Returns an empty log for a non-durable
// (in-memory) budget, which has no WAL.
func (bd *Budget) Log() ([]LogEntry, error) {
	if bd.dir == "" {
		return nil, nil // non-durable: no WAL, empty audit log
	}
	return ReadLog(bd.dir)
}

// ReadLog reads the full WAL at dir and returns its transitions in commit order.
// It is read-only — it opens no live budget and mutates nothing — so it is safe
// to call against a directory whose budget is also open in the daemon (the WAL
// file is append-only; a concurrent append just isn't seen by this read).
func ReadLog(dir string) ([]LogEntry, error) {
	var entries []LogEntry
	_, err := replayWAL(filepath.Join(dir, "wal.log"), 0, func(c Command) error {
		entries = append(entries, LogEntry{
			Kind:    commandKindName(c.Kind),
			JobID:   c.JobID,
			ArrayID: c.ArrayID,
			Idx:     c.Idx,
			N:       c.N,
			Rate:    c.C,
			W:       c.W,
			Runtime: c.Runtime,
			Elapsed: c.Elapsed,
			Amount:  c.Amount,
			TS:      c.TS,
			TE:      c.TE,
			Now:     c.Now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}
