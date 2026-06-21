package budget

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// snapshot mirrors all Budget state in serializable form. It records WALOffset
// = the WAL byte length this snapshot covers; recovery replays only records past
// that offset, so a crash between snapshot-write and any WAL truncation cannot
// double-apply.
type snapshot struct {
	// config
	C                 Units   `json:"c"`
	B0                Units   `json:"b0"`
	TS                Seconds `json:"ts"`
	TE                Seconds `json:"te"`
	BillInfraFailures bool    `json:"bif"`
	AllowRequeue      bool    `json:"areq"`
	K                 float64 `json:"k"`
	BurstEnabled      bool    `json:"be"`
	BurstCeilingPct   float64 `json:"bcp"`
	BurstDrawCap      Units   `json:"bdc"`
	// money ledger
	B             Units `json:"b"`
	ReservedTotal Units `json:"rt"`
	Consumed      Units `json:"cons"`
	WriteOff      Units `json:"wo"`
	Status        int   `json:"stat"`
	// burst ledger
	BurstPot  Units   `json:"bp"`
	FracAcc   Units   `json:"fa"`
	RLive     Units   `json:"rl"`
	LastTouch Seconds `json:"lt"`
	// live work
	Escrows []escrowSnap `json:"esc"`
	Arrays  []arraySnap  `json:"arr"`
	// recovery cursor
	WALOffset int64 `json:"waloff"`
}

type escrowSnap struct {
	JobID     string  `json:"j"`
	Reserved  Units   `json:"res"`
	W         Seconds `json:"w"`
	Started   bool    `json:"s"`
	BurstResv Units   `json:"br"`
}

type arraySnap struct {
	ArrayID   string     `json:"a"`
	C         Units      `json:"c"`
	W         Seconds    `json:"w"`
	N         int        `json:"n"`
	Remaining int        `json:"rem"`
	Reserved  Units      `json:"res"`
	Tasks     []taskSnap `json:"t"`
}

type taskSnap struct {
	Idx       int   `json:"i"`
	Started   bool  `json:"s"`
	Settled   bool  `json:"st"`
	BurstResv Units `json:"br"`
}

// captureSnapshot builds a snapshot of the current state. Caller holds bd.mu.
func (bd *Budget) captureSnapshot(walOffset int64) snapshot {
	s := snapshot{
		C: bd.C, B0: bd.B0, TS: bd.TS, TE: bd.TE,
		BillInfraFailures: bd.BillInfraFailures, AllowRequeue: bd.AllowRequeue, K: bd.K,
		BurstEnabled: bd.BurstEnabled, BurstCeilingPct: bd.BurstCeilingPct, BurstDrawCap: bd.BurstDrawCap,
		B: bd.B, ReservedTotal: bd.ReservedTotal, Consumed: bd.Consumed, WriteOff: bd.WriteOff,
		Status:   int(bd.status),
		BurstPot: bd.burstPot, FracAcc: bd.fracAcc, RLive: bd.rLive, LastTouch: bd.lastTouch,
		WALOffset: walOffset,
	}
	for _, e := range bd.escrows {
		s.Escrows = append(s.Escrows, escrowSnap{e.JobID, e.Reserved, e.W, e.Started, e.BurstResv})
	}
	for _, ae := range bd.arrays {
		as := arraySnap{ae.ArrayID, ae.C, ae.W, ae.N, ae.Remaining, ae.Reserved, nil}
		for _, ts := range ae.tasks {
			as.Tasks = append(as.Tasks, taskSnap{ts.idx, ts.started, ts.settled, ts.burstResv})
		}
		s.Arrays = append(s.Arrays, as)
	}
	return s
}

// budgetFromSnapshot rebuilds a Budget (sans wal/dir) from a snapshot.
func budgetFromSnapshot(s snapshot) *Budget {
	bd := &Budget{
		C: s.C, B: s.B, B0: s.B0, TS: s.TS, TE: s.TE,
		ReservedTotal: s.ReservedTotal, Consumed: s.Consumed, WriteOff: s.WriteOff,
		BillInfraFailures: s.BillInfraFailures, AllowRequeue: s.AllowRequeue, K: s.K,
		BurstEnabled: s.BurstEnabled, BurstCeilingPct: s.BurstCeilingPct, BurstDrawCap: s.BurstDrawCap,
		burstPot: s.BurstPot, fracAcc: s.FracAcc, rLive: s.RLive, lastTouch: s.LastTouch,
		status:  Status(s.Status),
		escrows: make(map[string]*Escrow),
		arrays:  make(map[string]*ArrayEscrow),
	}
	for _, e := range s.Escrows {
		bd.escrows[e.JobID] = &Escrow{JobID: e.JobID, Reserved: e.Reserved, W: e.W, Started: e.Started, BurstResv: e.BurstResv}
	}
	for _, as := range s.Arrays {
		ae := &ArrayEscrow{ArrayID: as.ArrayID, C: as.C, W: as.W, N: as.N, Remaining: as.Remaining, Reserved: as.Reserved, tasks: make(map[int]*taskState)}
		for _, t := range as.Tasks {
			ae.tasks[t.Idx] = &taskState{idx: t.Idx, started: t.Started, settled: t.Settled, burstResv: t.BurstResv}
		}
		bd.arrays[as.ArrayID] = ae
	}
	return bd
}

func saveSnapshot(dir string, s snapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "snapshot.json.tmp")
	final := filepath.Join(dir, "snapshot.json")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: daemon-owned path
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, final) // atomic: snapshot.json is old or new, never partial
}

func loadSnapshot(dir string) (snapshot, bool, error) {
	// dir is the daemon-owned state directory, never user input.
	data, err := os.ReadFile(filepath.Join(dir, "snapshot.json")) //nolint:gosec // G304: daemon-owned path
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot{}, false, nil
		}
		return snapshot{}, false, err
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return snapshot{}, false, err
	}
	return s, true, nil
}
