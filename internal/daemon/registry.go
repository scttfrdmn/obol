package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
)

// ErrNoBudget is returned by Resolve when an account maps to no configured
// budget. The gate rejects such a submission (SEAM_DESIGN.md §9: "none resolves
// → reject"); the shim then applies its per-partition fail policy.
var ErrNoBudget = errors.New("no budget for account")

// Registry holds one independent budget per account (the flat per-account model,
// SEAM_DESIGN.md §9). Resolution is an exact account-name match — sub-accounts
// with no entry do not roll up. Budgets are created/opened at startup, one WAL +
// snapshot directory each; the kernel is untouched.
type Registry struct {
	budgets     map[string]*budget.Budget
	access      map[string]AccountConfig // per-account allow-lists (access.go)
	adminUsers  []string                 // admins for mutating verbs (root is always admin)
	adminGroups []string
}

// adminEnforced reports whether an admin allow-list is set (mutating-verb authz on).
func (r *Registry) adminEnforced() bool {
	return len(r.adminUsers) > 0 || len(r.adminGroups) > 0
}

// nowFunc returns epoch seconds; injected so a fresh budget's window anchors to
// the same clock the server feeds transitions (mirrors cmd/obold's openOrCreate).
type nowFunc func() budget.Seconds

// NewRegistry opens-or-creates a budget per account under stateDir/<name>/. sync
// controls WAL fdatasync (production true). now anchors freshly-created windows.
func NewRegistry(cfg *Config, stateDir string, sync bool, now nowFunc) (*Registry, error) {
	r := &Registry{
		budgets:     make(map[string]*budget.Budget, len(cfg.Accounts)),
		access:      make(map[string]AccountConfig, len(cfg.Accounts)),
		adminUsers:  cfg.AdminUsers,
		adminGroups: cfg.AdminGroups,
	}
	for _, a := range cfg.Accounts {
		bd, err := openOrCreateAccount(stateDir, a, sync, now)
		if err != nil {
			_ = r.Close() // roll back any already-opened WALs
			return nil, fmt.Errorf("account %q: %w", a.Name, err)
		}
		r.budgets[a.Name] = bd
		r.access[a.Name] = a
	}
	return r, nil
}

// openOrCreateAccount recovers an account's budget from its dir, or creates it
// fresh (window anchored at [now, now+window)). Mirrors cmd/obold.openOrCreate
// but always creates when absent — the config is the source of truth for which
// accounts exist.
func openOrCreateAccount(stateDir string, a AccountConfig, sync bool, now nowFunc) (*budget.Budget, error) {
	dir := filepath.Join(stateDir, a.Name)
	if bd, err := budget.OpenBudget(dir, sync); err == nil {
		return bd, nil
	}
	win, err := a.windowOrDefault()
	if err != nil {
		return nil, err
	}
	start := now()
	secs := budget.Seconds(win / time.Second)
	return budget.NewDurable(dir, a.Rate, a.Balance, start, start+secs, sync)
}

// Resolve returns the budget for an account, or ErrNoBudget if none is
// configured under that exact name.
//
// Back-compat: a single-budget daemon (exactly one account, the synthesized
// "default") accepts ANY account name — there is only one pot, so a job's
// --account is irrelevant, exactly as before multi-account resolution existed.
// Multi-account registries require an exact match.
func (r *Registry) Resolve(account string) (*budget.Budget, error) {
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

// Names returns the configured account names (stable order not guaranteed).
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.budgets))
	for n := range r.budgets {
		names = append(names, n)
	}
	return names
}

// Single returns the sole budget when exactly one account is configured, used by
// status/inspection when no account is specified. ok is false if there are 0 or
// >1 accounts.
func (r *Registry) Single() (name string, bd *budget.Budget, ok bool) {
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
	var first error
	for _, bd := range r.budgets {
		if err := bd.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
