package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
)

// ErrNoBudget is returned by Resolve when an account maps to no configured
// budget. The gate rejects such a submission (SEAM_DESIGN.md §9: "none resolves
// → reject"); the shim then applies its per-partition fail policy.
var ErrNoBudget = errors.New("no budget for account")

// ErrExists is returned by Create when the account already exists.
var ErrExists = errors.New("account already exists")

// Registry holds one independent budget per account (the flat per-account model,
// SEAM_DESIGN.md §9). Resolution is an exact account-name match — sub-accounts
// with no entry do not roll up. Each account is one WAL+snapshot directory plus a
// daemon-owned account.json (name + access lists); the kernel is untouched.
//
// The registry is mutable at runtime (obol create/attach/detach): mu guards the
// maps — reads take RLock, mutations take Lock. The per-budget kernel locks are
// independent and unchanged.
type Registry struct {
	mu       sync.RWMutex
	budgets  map[string]*budget.Budget
	access   map[string]AccountConfig // per-account allow-lists (access.go)
	stateDir string
	sync     bool
	now      nowFunc

	adminUsers  []string // admins for mutating verbs (root is always admin)
	adminGroups []string
}

// adminEnforced reports whether an admin allow-list is set (mutating-verb authz on).
func (r *Registry) adminEnforced() bool {
	return len(r.adminUsers) > 0 || len(r.adminGroups) > 0
}

// nowFunc returns epoch seconds; injected so a fresh budget's window anchors to
// the same clock the server feeds transitions (mirrors cmd/obold's openOrCreate).
type nowFunc func() budget.Seconds

// NewRegistry builds the registry from two sources, with on-disk state winning:
// (1) DISCOVERY — every existing account dir under stateDir (a dir with a
// snapshot) is opened and its account.json metadata loaded; (2) CONFIG — any
// account listed in cfg but not already on disk is created. This makes a
// runtime-created account survive a restart without rewriting the config.
func NewRegistry(cfg *Config, stateDir string, sync bool, now nowFunc) (*Registry, error) {
	r := &Registry{
		budgets:     make(map[string]*budget.Budget),
		access:      make(map[string]AccountConfig),
		stateDir:    stateDir,
		sync:        sync,
		now:         now,
		adminUsers:  cfg.AdminUsers,
		adminGroups: cfg.AdminGroups,
	}
	// (1) Discover existing account dirs.
	if err := r.discover(); err != nil {
		_ = r.Close()
		return nil, err
	}
	// (2) Bootstrap config accounts not already on disk.
	for _, a := range cfg.Accounts {
		if _, ok := r.budgets[a.Name]; ok {
			continue // on-disk state wins
		}
		if err := r.create(a); err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("account %q: %w", a.Name, err)
		}
	}
	return r, nil
}

// discover scans stateDir for existing account directories (each has a
// snapshot.json) and registers them, loading account.json for name + access.
func (r *Registry) discover() error {
	entries, err := os.ReadDir(r.stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh state dir; nothing to discover
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.stateDir, e.Name())
		if _, statErr := os.Stat(filepath.Join(dir, "snapshot.json")); statErr != nil {
			continue // not an obol account dir
		}
		bd, err := budget.OpenBudget(dir, r.sync)
		if err != nil {
			return fmt.Errorf("recover account %q: %w", e.Name(), err)
		}
		meta, err := readAccountMeta(dir)
		if err != nil {
			return fmt.Errorf("account %q meta: %w", e.Name(), err)
		}
		r.budgets[meta.Name] = bd
		r.access[meta.Name] = meta
	}
	return nil
}

// create opens-or-creates the account's budget dir, writes its metadata, and
// registers it. Caller holds mu.Lock (or is in single-threaded startup).
func (r *Registry) create(a AccountConfig) error {
	dir := filepath.Join(r.stateDir, a.Name)
	bd, err := budget.OpenBudget(dir, r.sync)
	if err != nil {
		win, werr := a.windowOrDefault()
		if werr != nil {
			return werr
		}
		start := r.now()
		secs := budget.Seconds(win / time.Second)
		if bd, err = budget.NewDurable(dir, a.Rate, a.Balance, start, start+secs, r.sync); err != nil {
			return err
		}
	}
	if err := writeAccountMeta(dir, a); err != nil {
		_ = bd.Close()
		return err
	}
	r.budgets[a.Name] = bd
	r.access[a.Name] = a
	return nil
}

// Create adds a new account at runtime (obol create). Rejects a duplicate name.
func (r *Registry) Create(a AccountConfig) error {
	if r.now == nil || r.stateDir == "" {
		// A single-budget daemon (obold without -config) has no state dir/clock for
		// live creation; runtime create requires -config.
		return fmt.Errorf("runtime account creation requires obold -config")
	}
	if a.Name == "" {
		return fmt.Errorf("account name required")
	}
	if a.Rate <= 0 {
		return fmt.Errorf("rate must be positive")
	}
	if a.Balance < 0 {
		return fmt.Errorf("balance must be non-negative")
	}
	if _, err := a.windowOrDefault(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.budgets[a.Name]; ok {
		return fmt.Errorf("%w: %q", ErrExists, a.Name)
	}
	return r.create(a)
}

// SetAccess replaces an account's allow-lists and persists them. Caller-facing
// mutation for attach/detach. Returns ErrNoBudget for an unknown account.
func (r *Registry) SetAccess(account string, users, groups []string) error {
	if r.stateDir == "" {
		return fmt.Errorf("runtime access changes require obold -config")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.access[account]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoBudget, account)
	}
	a.AllowUsers = users
	a.AllowGroups = groups
	if err := writeAccountMeta(filepath.Join(r.stateDir, account), a); err != nil {
		return err
	}
	r.access[account] = a
	return nil
}

// accessOf returns a copy of an account's config (allow-lists) under RLock, for
// the authorizer. ok is false if the account is unknown.
func (r *Registry) accessOf(account string) (AccountConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.access[account]
	return a, ok
}

// Resolve returns the budget for an account, or ErrNoBudget if none exists.
//
// Back-compat: a single-budget daemon (exactly one account, the synthesized
// "default") accepts ANY account name — there is only one pot, so a job's
// --account is irrelevant, exactly as before multi-account resolution existed.
func (r *Registry) Resolve(account string) (*budget.Budget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if bd, ok := r.budgets[account]; ok {
		return bd, nil
	}
	if len(r.budgets) == 1 {
		if bd, ok := r.budgets["default"]; ok {
			return bd, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNoBudget, account)
}

// Names returns the account names (stable order not guaranteed).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.budgets))
	for n := range r.budgets {
		names = append(names, n)
	}
	return names
}

// Single returns the sole budget when exactly one account exists, used by
// status/inspection when no account is specified. ok is false for 0 or >1.
func (r *Registry) Single() (name string, bd *budget.Budget, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.budgets) != 1 {
		return "", nil, false
	}
	for n, b := range r.budgets {
		return n, b, true
	}
	return "", nil, false
}

// Close closes every account's WAL (each Close flushes under group commit),
// returning the first error encountered.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, bd := range r.budgets {
		if err := bd.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
