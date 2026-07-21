package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the obold multi-account configuration (obold -config <path>). Each
// account is an independent budget (docs/SEAM_DESIGN.md §9): a submission's
// Slurm --account resolves to exactly one account's budget. There is no account
// tree and no rollup — each account conserves on its own.
type Config struct {
	Accounts []AccountConfig `json:"accounts"`
}

// AccountConfig describes one account's budget and optional access restriction.
type AccountConfig struct {
	Name    string `json:"name"`             // Slurm account name (the resolution key)
	Balance int64  `json:"balance"`          // initial allocation (B0)
	Rate    int64  `json:"rate"`             // flat cost per second; TRES weights (if set) override per job
	Window  string `json:"window,omitempty"` // budget window as a Go duration (default 720h = 30d)

	// Optional access restriction. Empty = open: obol trusts that Slurm already
	// authorized the submitter's account membership (SEAM §9 — no parallel
	// identity store). If either list is non-empty, obol additionally requires the
	// submitter's user or one of its groups to be listed.
	AllowUsers  []string `json:"allow_users,omitempty"`
	AllowGroups []string `json:"allow_groups,omitempty"`
}

// windowOrDefault parses Window, defaulting to 30 days.
func (a AccountConfig) windowOrDefault() (time.Duration, error) {
	if a.Window == "" {
		return 30 * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(a.Window)
	if err != nil {
		return 0, fmt.Errorf("account %q: bad window %q: %w", a.Name, a.Window, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("account %q: window must be positive", a.Name)
	}
	return d, nil
}

// restricted reports whether this account enforces an obol-side access list.
func (a AccountConfig) restricted() bool {
	return len(a.AllowUsers) > 0 || len(a.AllowGroups) > 0
}

// LoadConfig reads and validates a Config from path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: daemon operator supplies the path
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a typo'd key is a config error, not silently ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate checks the config is usable: at least one account, unique names,
// non-negative balances, positive rate, parseable windows.
func (c *Config) validate() error {
	if len(c.Accounts) == 0 {
		return fmt.Errorf("config has no accounts")
	}
	seen := make(map[string]bool, len(c.Accounts))
	for _, a := range c.Accounts {
		if a.Name == "" {
			return fmt.Errorf("account with empty name")
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate account %q", a.Name)
		}
		seen[a.Name] = true
		if a.Balance < 0 {
			return fmt.Errorf("account %q: negative balance", a.Name)
		}
		if a.Rate <= 0 {
			return fmt.Errorf("account %q: rate must be positive", a.Name)
		}
		if _, err := a.windowOrDefault(); err != nil {
			return err
		}
	}
	return nil
}
