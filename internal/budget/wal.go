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
	W       Seconds `json:"w,omitempty"`
	Runtime Seconds `json:"r,omitempty"`
	Elapsed Seconds `json:"e,omitempty"`
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
)

// WAL is an append-only command log. Record framing: [u32 len][u32 crc32][payload].
// A torn tail (partial write from a crash) fails the length/crc check on replay
// and is discarded.
type WAL struct {
	mu    sync.Mutex
	f     *os.File
	sync  bool
	bytes int64 // total bytes durably appended (== file size); read for snapshot offset
}

// OpenWAL opens (creating if absent) the append-only command log at path. When
// sync is true every Append fdatasyncs. The current file size becomes the
// starting durable byte count.
func OpenWAL(path string, sync bool) (*WAL, error) {
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
	return &WAL{f: f, sync: sync, bytes: info.Size()}, nil
}

// Size returns the total bytes durably appended (the file size), used as the
// snapshot's recovery offset.
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// Append marshals and writes one committed command, fdatasyncing when sync is
// set. The record framing is [u32 len][u32 crc32][payload].
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
	defer w.mu.Unlock()
	if _, err := w.f.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.f.Write(payload); err != nil {
		return err
	}
	if w.sync {
		if err := w.f.Sync(); err != nil { // production: fdatasync; group-commit is the next optimization
			return err
		}
	}
	w.bytes += int64(len(hdr) + len(payload))
	return nil
}

// Close closes the underlying file.
func (w *WAL) Close() error { return w.f.Close() }

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
