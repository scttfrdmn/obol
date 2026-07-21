package budget

import (
	"fmt"
	"os"
	"path/filepath"
)

// logCmd appends a committed transition to the WAL. Called under bd.mu, after
// validation passes and before the mutation commits. No-op when durability is
// off or during replay.
func (bd *Budget) logCmd(c Command) error {
	if bd.replaying || bd.wal == nil {
		return nil
	}
	return bd.wal.Append(c)
}

// NewDurable creates a fresh durable budget in dir, writing an initial snapshot
// (which captures config) and opening an empty WAL. `sync` fdatasyncs every
// append (production); pass false for throughput in tests.
func NewDurable(dir string, c, b0, ts, te Units, sync bool) (*Budget, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	bd := New(c, b0, ts, te)
	bd.dir = dir
	// Initial snapshot at WAL offset 0.
	if err := saveSnapshot(dir, bd.captureSnapshot(0)); err != nil {
		return nil, err
	}
	wal, err := OpenWAL(filepath.Join(dir, "wal.log"), sync)
	if err != nil {
		return nil, err
	}
	bd.wal = wal
	return bd, nil
}

// OpenBudget recovers a budget from dir: load the latest snapshot, then replay
// WAL records past the snapshot's covered offset through the same transition
// methods. After replay, conservation is asserted — a violation means corruption.
func OpenBudget(dir string, sync bool) (*Budget, error) {
	s, ok, err := loadSnapshot(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no snapshot in %s; use NewDurable to create", dir)
	}
	bd := budgetFromSnapshot(s)
	bd.dir = dir

	// Replay WAL records committed after the snapshot offset.
	bd.replaying = true
	endOff, rerr := replayWAL(filepath.Join(dir, "wal.log"), s.WALOffset, func(c Command) error {
		return bd.applyCommand(c)
	})
	bd.replaying = false
	if rerr != nil {
		return nil, fmt.Errorf("WAL replay failed: %w", rerr)
	}
	_ = endOff

	if ok, sum := bd.ConservationOKArrays(); !ok {
		return nil, fmt.Errorf("recovery violated conservation: B0=%d got=%d", bd.B0, sum)
	}

	wal, err := OpenWAL(filepath.Join(dir, "wal.log"), sync)
	if err != nil {
		return nil, err
	}
	bd.wal = wal
	return bd, nil
}

// Snapshot captures current state atomically and records the current WAL offset
// so recovery replays only newer records. Old WAL records before the offset are
// now redundant (a prefix-truncation could reclaim them; we keep them for now).
func (bd *Budget) Snapshot() error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if bd.wal == nil {
		return fmt.Errorf("not a durable budget")
	}
	off := bd.wal.Size()
	return saveSnapshot(bd.dir, bd.captureSnapshot(off))
}

// Close releases the WAL file handle. Safe to call on a non-durable budget.
func (bd *Budget) Close() error {
	if bd.wal != nil {
		return bd.wal.Close()
	}
	return nil
}

// applyCommand re-executes a logged command through the same public methods used
// live. replaying is true, so these skip logCmd. A non-nil return signals a
// command that failed to re-apply — corruption or a logic bug, surfaced loudly.
func (bd *Budget) applyCommand(c Command) error {
	switch c.Kind {
	case KindSubmit:
		return bd.SubmitAt(c.JobID, c.C, c.W, c.Now)
	case KindStart:
		return bd.Start(c.JobID, c.Now)
	case KindComplete:
		return bd.Complete(c.JobID, c.Runtime, c.Now)
	case KindTimeout:
		return bd.Timeout(c.JobID, c.Now)
	case KindCancel:
		return bd.Cancel(c.JobID, c.Elapsed, c.Now)
	case KindInfraFail:
		return bd.InfraFail(c.JobID, c.Elapsed, c.Now)
	case KindSubmitArray:
		return bd.SubmitArrayAt(c.ArrayID, c.C, c.N, c.W, c.Now)
	case KindStartTask:
		return bd.StartTask(c.ArrayID, c.Idx, c.Now)
	case KindCompleteTask:
		return bd.CompleteTask(c.ArrayID, c.Idx, c.Runtime, c.Now)
	case KindTimeoutTask:
		return bd.TimeoutTask(c.ArrayID, c.Idx, c.Now)
	case KindCancelTask:
		return bd.CancelTask(c.ArrayID, c.Idx, c.Elapsed, c.Now)
	case KindInfraFailTask:
		return bd.InfraFailTask(c.ArrayID, c.Idx, c.Elapsed, c.Now)
	case KindLapse:
		bd.Lapse()
		return nil
	case KindTopUp:
		return bd.TopUpXfer(c.Amount, c.Xfer, c.Now)
	case KindWithdraw:
		return bd.WithdrawXfer(c.Amount, c.Xfer, c.Now)
	case KindReprice:
		return bd.Reprice(c.JobID, c.C, c.Now)
	case KindSetRate:
		return bd.SetRate(c.C, c.Now)
	case KindSetWindow:
		return bd.SetWindow(c.TS, c.TE, c.Now)
	default:
		return fmt.Errorf("unknown command kind %q", c.Kind)
	}
}
