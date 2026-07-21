package budget

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sync"
)

// Command is a logged, replayable transition. Every mutating method maps to one.
// All non-determinism is captured in fields (notably Now), so replaying the
// command stream through the same methods reproduces state exactly.
type Command struct {
	Kind    string  `json:"k"`
	JobID   string  `json:"j,omitempty"`
	ArrayID string  `json:"a,omitempty"`
	Idx     int     `json:"i,omitempty"`
	N       int     `json:"n,omitempty"`
	C       Units   `json:"c,omitempty"` // per-job cost rate frozen at submit (0 = budget flat rate)
	W       Seconds `json:"w,omitempty"`
	Runtime Seconds `json:"r,omitempty"`
	Elapsed Seconds `json:"e,omitempty"`
	Amount  Units   `json:"amt,omitempty"` // top-up / withdraw amount (KindTopUp/KindWithdraw)
	TS      Seconds `json:"ts,omitempty"`  // window start (KindSetWindow)
	TE      Seconds `json:"te,omitempty"`  // window end (KindSetWindow)
	Xfer    string  `json:"x,omitempty"`   // transfer id tagging a topup/withdraw leg (obol transfer, #25)
	Now     Seconds `json:"t,omitempty"`
}

// Command kinds. Each names one mutating transition; the WAL records these so
// replay re-executes the same methods.
const (
	KindSubmit        = "sub"
	KindStart         = "st"
	KindComplete      = "cmp"
	KindTimeout       = "to"
	KindCancel        = "can"
	KindInfraFail     = "if"
	KindSubmitArray   = "asub"
	KindStartTask     = "ast"
	KindCompleteTask  = "acmp"
	KindTimeoutTask   = "ato"
	KindCancelTask    = "acan"
	KindInfraFailTask = "aif"
	KindLapse         = "lap"
	KindTopUp         = "top"
	KindWithdraw      = "wd"
	KindReprice       = "rep"
	KindSetRate       = "srate"
	KindSetWindow     = "swin"
)

// WAL is an append-only command log. Record framing: [u32 len][u32 crc32][payload].
// A torn tail (partial write from a crash) fails the length/crc check on replay
// and is discarded.
//
// Durability uses GROUP COMMIT. Append writes the record to the file (page
// cache) and returns immediately, before its bytes are fsynced — the record is
// on disk-cache before any in-memory mutation the caller makes, so the torn-tail
// discipline (invariant #4) is preserved: a crash before the fsync loses both
// the un-synced tail and the caller's still-in-memory mutation together. A
// background committer batches fsyncs: while one fsync (slow) is in flight, more
// appends (fast, page-cache writes) accumulate and the next fsync covers them
// all in one syscall. This keeps the fsync off the caller's lock — the GATE ack
// returns after the in-memory escrow; durability lands a hair later (§3/§13.3).
type WAL struct {
	mu        sync.Mutex
	f         *os.File
	syncOn    bool
	writeOff  int64 // bytes written to the file (page cache); == file size, snapshot offset
	syncedOff int64 // bytes durably fsynced (<= writeOff)
	syncErr   error // sticky: a background fsync that failed, surfaced on Close/Flush

	cond    *sync.Cond    // broadcast when syncedOff advances or syncErr is set
	flushCh chan struct{} // buffered(1) nudge: "there is unsynced data"
	doneCh  chan struct{} // closed by Close to stop the committer
	wg      sync.WaitGroup
}

// OpenWAL opens (creating if absent) the append-only command log at path. When
// sync is true a background committer group-commits fsyncs; when false, Append
// never fsyncs (throughput mode for tests). The current file size becomes the
// starting offset (an existing file is already durable).
func OpenWAL(path string, syncOn bool) (*WAL, error) {
	// path is the daemon-owned WAL path, never user input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // G304: daemon-owned path
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w := &WAL{
		f: f, syncOn: syncOn,
		writeOff: info.Size(), syncedOff: info.Size(),
		flushCh: make(chan struct{}, 1),
		doneCh:  make(chan struct{}),
	}
	w.cond = sync.NewCond(&w.mu)
	if syncOn {
		w.wg.Add(1)
		go w.committer()
	}
	return w, nil
}

// Size returns bytes written to the file (== file size), used as the snapshot's
// recovery offset. Using the written (not synced) offset is correct: a snapshot
// taken at writeOff already reflects every op whose record is below that offset,
// so on recovery those ops are in the snapshot even if their WAL bytes weren't
// synced — replay from writeOff simply finds EOF and applies nothing extra.
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeOff
}

// SyncedSize returns bytes durably fsynced. Introspection / tests.
func (w *WAL) SyncedSize() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncedOff
}

// Append marshals and writes one committed command to the page cache, then
// returns. The fsync is deferred to the background committer (group commit); in
// non-sync mode there is no fsync at all. Framing: [u32 len][u32 crc32][payload].
func (w *WAL) Append(c Command) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(payload) > math.MaxUint32 {
		// A marshaled Command is tiny; this can't happen, but the framing uses a
		// u32 length, so guard rather than silently wrap.
		return fmt.Errorf("wal: record too large (%d bytes)", len(payload))
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload))) //nolint:gosec // G115: bounded by the check above
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.ChecksumIEEE(payload))

	w.mu.Lock()
	if w.syncErr != nil {
		err := w.syncErr
		w.mu.Unlock()
		return err // a prior background fsync failed; refuse further appends
	}
	if _, err := w.f.Write(hdr[:]); err != nil {
		w.mu.Unlock()
		return err
	}
	if _, err := w.f.Write(payload); err != nil {
		w.mu.Unlock()
		return err
	}
	w.writeOff += int64(len(hdr) + len(payload))
	if !w.syncOn {
		w.syncedOff = w.writeOff // no durability guarantee in throughput mode
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()
	// Nudge the committer that there is unsynced data (coalesced).
	select {
	case w.flushCh <- struct{}{}:
	default:
	}
	return nil
}

// committer is the group-commit loop: on each nudge (or on shutdown) it fsyncs,
// advancing syncedOff to whatever had been written when the fsync began. While a
// slow fsync runs, more appends accumulate and are covered by the next pass.
func (w *WAL) committer() {
	defer w.wg.Done()
	for {
		select {
		case <-w.doneCh:
			return
		case <-w.flushCh:
		}
		w.flushOnce()
	}
}

// flushOnce fsyncs if there is unsynced data, advancing syncedOff (or recording
// a sticky error). The fsync itself runs OFF the mutex so appends never block on
// it — that is the whole point.
func (w *WAL) flushOnce() {
	w.mu.Lock()
	if w.syncErr != nil {
		w.mu.Unlock()
		return
	}
	target := w.writeOff
	if w.syncedOff >= target {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	err := w.f.Sync()

	w.mu.Lock()
	if err != nil {
		w.syncErr = err
	} else if target > w.syncedOff {
		w.syncedOff = target
	}
	w.cond.Broadcast()
	w.mu.Unlock()
}

// Flush forces a synchronous fsync of everything written so far and returns any
// sync error. Used by Close and available for callers that need a durability
// barrier (e.g. before a snapshot that will truncate the WAL).
func (w *WAL) Flush() error {
	if !w.syncOn {
		return nil
	}
	w.mu.Lock()
	target := w.writeOff
	w.mu.Unlock()
	w.flushOnce()
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.syncErr == nil && w.syncedOff < target {
		w.cond.Wait()
	}
	return w.syncErr
}

// Close stops the committer, does a final synchronous fsync, and closes the
// file. It returns any sticky sync error so a durability failure is not silently
// swallowed at shutdown.
func (w *WAL) Close() error {
	if w.syncOn {
		close(w.doneCh)
		w.wg.Wait()   // committer has stopped; safe to fsync/close the fd
		w.flushOnce() // final durability barrier
	}
	w.mu.Lock()
	serr := w.syncErr
	w.mu.Unlock()
	cerr := w.f.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// replayWAL reads records starting at byteOffset, invoking fn for each intact
// record. It stops cleanly at EOF or at the first torn/corrupt record (treating
// the tail as never-committed), returning the offset of the last good record's end.
func replayWAL(path string, byteOffset int64, fn func(Command) error) (int64, error) {
	// path is the daemon-owned WAL path (filepath.Join of a configured dir and a
	// fixed name), never user input.
	f, err := os.Open(path) //nolint:gosec // G304: daemon-owned path
	if err != nil {
		if os.IsNotExist(err) {
			return byteOffset, nil
		}
		return byteOffset, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
		return byteOffset, err
	}
	r := bufio.NewReader(f)
	good := byteOffset
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return good, nil // clean end or torn header -> stop, discard tail
			}
			return good, err
		}
		n := binary.LittleEndian.Uint32(hdr[0:4])
		want := binary.LittleEndian.Uint32(hdr[4:8])
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return good, nil // torn payload -> discard tail
		}
		if crc32.ChecksumIEEE(payload) != want {
			return good, nil // corrupt record -> discard tail
		}
		var c Command
		if err := json.Unmarshal(payload, &c); err != nil {
			return good, nil
		}
		if err := fn(c); err != nil {
			return good, err // a replayed command that fails to apply = corruption/bug; surface it
		}
		good += int64(len(hdr)) + int64(n)
	}
}
