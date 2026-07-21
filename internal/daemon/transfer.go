package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/scttfrdmn/obol/internal/budget"
)

// Transfer moves money between two account budgets atomically across a crash
// (obol transfer, #25). It is the first operation to touch two kernels, so it is
// engineered around invariant #1 (conservation) holding across BOTH budgets, not
// just each one.
//
// The hazard: a transfer is two legs — from.Withdraw then to.TopUp — each
// individually crash-safe via its own WAL. A crash after the withdraw's fsync and
// before the topup's would DESTROY the in-flight amount. To close that window,
// each leg's WAL command is tagged with a transfer id (Xfer), and a daemon
// journal record names the transfer. On restart, recoverTransfers scans both
// WALs for the id and applies whichever leg is missing (see budget.HasXfer),
// making each leg exactly-once. Money is never created or destroyed.
//
// The two legs run SEQUENTIALLY (withdraw fully commits before topup begins), so
// only one kernel lock is ever held at a time — no lock-ordering deadlock.

// transferDir is the subdir under stateDir holding in-flight transfer journal
// records. A record exists only for the duration of a transfer (and after a
// crash, until recovery completes it); a clean run deletes it.
const transferDir = "transfers"

// transferRecord is one in-flight transfer's journal entry. Now is stamped from
// the registry clock at journal time and reused for BOTH legs, so replay is a
// pure function of (state, command, now) — invariant #3.
type transferRecord struct {
	XferID string         `json:"xfer_id"`
	From   string         `json:"from"`
	To     string         `json:"to"`
	Amount budget.Units   `json:"amount"`
	Now    budget.Seconds `json:"now"`
}

// mintXferID returns a unique transfer id. Mirrors server.mintToken but with a
// "xfer:" prefix so a journal id is never confused with an escrow token.
func mintXferID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return "xfer:" + hex.EncodeToString(b[:]), nil
}

// writeTransferRecord atomically writes a journal record (temp + rename + fsync),
// the same durability discipline as the kernel snapshot. The fsync ensures the
// record is on disk BEFORE the first leg mutates, so a crash mid-transfer always
// leaves a record for recovery to find.
func writeTransferRecord(stateDir string, rec transferRecord) error {
	dir := filepath.Join(stateDir, transferDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, sanitizeXfer(rec.XferID)+".json.tmp")
	final := filepath.Join(dir, sanitizeXfer(rec.XferID)+".json")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // daemon-owned path
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // durability barrier before the first leg
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, final) // atomic: the record is old-absent or fully present
}

// deleteTransferRecord removes a completed transfer's journal record.
func deleteTransferRecord(stateDir, xferID string) error {
	err := os.Remove(filepath.Join(stateDir, transferDir, sanitizeXfer(xferID)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// sanitizeXfer maps a transfer id to a safe filename. The id is daemon-minted
// hex with a "xfer:" prefix; strip the colon so it's a clean base name.
func sanitizeXfer(xferID string) string {
	out := make([]rune, 0, len(xferID))
	for _, r := range xferID {
		if r == ':' || r == '/' || r == os.PathSeparator {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// Transfer moves amount (or the whole available balance when all is true) from
// one account to another, journaled for crash atomicity. It returns the amount
// actually moved. Admin authorization is enforced by the caller (handleTransfer).
func (r *Registry) Transfer(from, to string, amount budget.Units, all bool) (budget.Units, error) {
	if r.stateDir == "" || r.now == nil {
		return 0, fmt.Errorf("transfers require obold -config")
	}
	if from == to {
		return 0, fmt.Errorf("cannot transfer to the same account %q", from)
	}
	// Resolve both budgets under the registry read lock; the per-budget kernel
	// locks are taken later, one at a time, by each leg.
	fromBd, err := r.Resolve(from)
	if err != nil {
		return 0, fmt.Errorf("source %w", err)
	}
	toBd, err := r.Resolve(to)
	if err != nil {
		return 0, fmt.Errorf("destination %w", err)
	}

	// Decide the amount and pre-validate it BEFORE journaling, so we never write a
	// record for an impossible move. (The kernel Withdraw re-checks under its lock;
	// a concurrent drain between here and the leg simply fails the leg, and the
	// journal — written first — is cleaned up on the error path below.)
	amt := amount
	if all {
		amt = fromBd.Balance()
	}
	if amt <= 0 {
		return 0, fmt.Errorf("nothing to transfer (amount %d)", amt)
	}
	if bal := fromBd.Balance(); amt > bal {
		return 0, fmt.Errorf("source has %d available, cannot transfer %d", bal, amt)
	}

	xferID, err := mintXferID()
	if err != nil {
		return 0, err
	}
	now := r.now()
	rec := transferRecord{XferID: xferID, From: from, To: to, Amount: amt, Now: now}
	if err := writeTransferRecord(r.stateDir, rec); err != nil {
		return 0, fmt.Errorf("journal transfer: %w", err)
	}

	// Leg 1: withdraw from the source. On failure, drop the (now moot) journal
	// record and abort — no money moved.
	if err := fromBd.WithdrawXfer(amt, xferID, now); err != nil {
		_ = deleteTransferRecord(r.stateDir, xferID)
		return 0, fmt.Errorf("withdraw from %s: %w", from, err)
	}
	// Leg 2: deposit into the destination. A TopUp cannot fail on a valid budget
	// (add-only, positive amount), but if it somehow does, recovery will complete
	// it from the journal — we deliberately leave the record in place here.
	if err := toBd.TopUpXfer(amt, xferID, now); err != nil {
		return 0, fmt.Errorf("deposit to %s (transfer %s will be completed on restart): %w", to, xferID, err)
	}

	// Both legs committed; the transfer is durable in the two WALs. Drop the record.
	if err := deleteTransferRecord(r.stateDir, xferID); err != nil {
		return amt, fmt.Errorf("transfer committed but journal cleanup failed: %w", err)
	}
	return amt, nil
}

// recoverTransfers completes any transfer interrupted by a crash. For each
// journal record it inspects both WALs for the tagged legs and applies whichever
// is missing, then removes the record. Called from NewRegistry after discover(),
// while single-threaded (no lock needed). A record naming an unknown account is
// corruption and surfaces loudly rather than silently dropping money.
func (r *Registry) recoverTransfers() error {
	dir := filepath.Join(r.stateDir, transferDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no in-flight transfers
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue // skip stray temp files / subdirs
		}
		rec, err := readTransferRecord(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("read transfer journal %q: %w", e.Name(), err)
		}
		if err := r.completeTransfer(rec); err != nil {
			return err
		}
		if err := deleteTransferRecord(r.stateDir, rec.XferID); err != nil {
			return fmt.Errorf("clear recovered transfer %q: %w", rec.XferID, err)
		}
	}
	return nil
}

// completeTransfer resolves a journaled transfer interrupted by a crash. The two
// legs are applied SEQUENTIALLY in Transfer (withdraw commits before deposit
// begins), so a committed deposit always implies a committed withdraw. That
// ordering makes recovery a clean three-way decision on what the two WALs show:
//
//   - withdrew && deposited  → both committed; nothing to do (the record just
//     outlived a crash between the second commit and the journal delete).
//   - withdrew && !deposited → money left the source but never landed. It MUST be
//     completed or it is destroyed — apply the deposit.
//   - !withdrew              → no money moved (deposit cannot precede withdraw),
//     so the transfer safely never happened; abort it. This is also why we do
//     NOT re-apply the withdraw: a concurrent job-submit could have reduced the
//     source balance in the tiny window before the crash, and re-applying could
//     fail — but there is nothing to complete, so we don't try.
//
// A committed deposit with no committed withdraw is an impossible ordering →
// corruption, surfaced loudly rather than silently minting money.
func (r *Registry) completeTransfer(rec transferRecord) error {
	fromBd, ok := r.budgets[rec.From]
	if !ok {
		return fmt.Errorf("transfer %s names unknown source account %q", rec.XferID, rec.From)
	}
	toBd, ok := r.budgets[rec.To]
	if !ok {
		return fmt.Errorf("transfer %s names unknown destination account %q", rec.XferID, rec.To)
	}

	withdrew, err := budget.HasXfer(fromBd.Dir(), budget.KindWithdraw, rec.XferID)
	if err != nil {
		return err
	}
	deposited, err := budget.HasXfer(toBd.Dir(), budget.KindTopUp, rec.XferID)
	if err != nil {
		return err
	}

	switch {
	case !withdrew && deposited:
		return fmt.Errorf("transfer %s corrupt: deposit committed without withdraw", rec.XferID)
	case !withdrew:
		return nil // no money moved; abort (caller drops the journal record)
	case withdrew && !deposited:
		if err := toBd.TopUpXfer(rec.Amount, rec.XferID, rec.Now); err != nil {
			return fmt.Errorf("recover deposit leg of %s: %w", rec.XferID, err)
		}
	}
	return nil // withdrew && deposited: already whole
}

// readTransferRecord loads one journal record.
func readTransferRecord(path string) (transferRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // daemon-owned path
	if err != nil {
		return transferRecord{}, err
	}
	var rec transferRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return transferRecord{}, err
	}
	return rec, nil
}
