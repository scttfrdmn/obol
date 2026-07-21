package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
)

// Config is the obold multi-account configuration (obold -config <path>). Each
// account is an independent budget (docs/SEAM_DESIGN.md §9): a submission's
// Slurm --account resolves to exactly one account's budget. There is no account
// tree and no rollup — each account conserves on its own.
type Config struct {
	Accounts []AccountConfig `json:"accounts"`

	// Admins may run mutating management commands (topup; later create/move). The
	// check uses the connection's KERNEL-VERIFIED peer uid/gid (SO_PEERCRED), not
	// the wire uid. root (uid 0) is always an admin. When both lists are empty,
	// admin enforcement is OFF — the socket's file permissions are the boundary,
	// preserving pre-authz behavior. Setting either turns enforcement on.
	AdminUsers  []string `json:"admin_users,omitempty"`
	AdminGroups []string `json:"admin_groups,omitempty"`

	// Node-type cost model (issue #65). NodeTypes maps a node-type name to its
	// cost rate. Partitions maps a partition to the set of node types it can place
	// on. When a partition has node types configured, the gate escrows the
	// WORST-CASE (max) rate over that set at submit (placement is unknown), and
	// the daemon reprices to the actual node's rate at BIND. When a partition is
	// absent here, cost falls back to TRES weights / the account's flat rate — all
	// prior behavior intact.
	NodeTypes  map[string]NodeRate `json:"node_types,omitempty"`
	Partitions []PartitionConfig   `json:"partitions,omitempty"`
}

// NodeRate is a node type's cost, expressed as an amount per time unit for
// admin convenience (e.g. 250 per "h"). The kernel bills per second in integer
// money, so the unit must divide evenly into per-second — validated at load.
type NodeRate struct {
	Rate int64  `json:"rate"`          // cost per Per
	Per  string `json:"per,omitempty"` // "s" (default), "m", or "h"
}

// perSecond converts the rate to integer units/second, or errors if it does not
// divide cleanly (e.g. 250/h = 250/3600 is fractional). Keeps the kernel's
// exact-integer money invariant intact.
func (r NodeRate) perSecond() (int64, error) {
	var div int64
	switch r.Per {
	case "", "s":
		div = 1
	case "m":
		div = 60
	case "h":
		div = 3600
	default:
		return 0, fmt.Errorf("bad per %q (want s|m|h)", r.Per)
	}
	if r.Rate <= 0 {
		return 0, fmt.Errorf("rate must be positive")
	}
	if r.Rate%div != 0 {
		return 0, fmt.Errorf("rate %d per %q is not a whole number of units/second (%d does not divide %d)", r.Rate, r.Per, div, r.Rate)
	}
	return r.Rate / div, nil
}

// PartitionConfig lists the node types a partition can place on (issue #65).
type PartitionConfig struct {
	Name      string   `json:"name"`
	NodeTypes []string `json:"node_types"`
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

	// Optional burst token bucket (issue #14). Off by default. When enabled, jobs
	// pushing aggregate burn above the sustainable rate r0 = B0/window must reserve
	// banked "permission" tokens to dispatch (this is concurrency shaping, not
	// money — burst is a separate bounded ledger). BurstCeilingPct caps the pot at
	// a fraction of B0 (0 < pct <= 1); BurstDrawCap bounds one job's reservation
	// (0 = unlimited).
	BurstEnabled    bool    `json:"burst_enabled,omitempty"`
	BurstCeilingPct float64 `json:"burst_ceiling_pct,omitempty"`
	BurstDrawCap    int64   `json:"burst_draw_cap,omitempty"`
}

// burstConfig converts the account's burst settings into the kernel's BurstConfig.
func (a AccountConfig) burstConfig() budget.BurstConfig {
	return budget.BurstConfig{
		Enabled:    a.BurstEnabled,
		CeilingPct: a.BurstCeilingPct,
		DrawCap:    a.BurstDrawCap,
	}
}

// validateBurst checks the burst settings are coherent. Burst fields set with
// burst_enabled=false are a config error (loud, matching DisallowUnknownFields),
// not silently ignored.
func (a AccountConfig) validateBurst() error {
	if !a.BurstEnabled {
		if a.BurstCeilingPct != 0 || a.BurstDrawCap != 0 {
			return fmt.Errorf("account %q: burst_ceiling_pct/burst_draw_cap set but burst_enabled is false", a.Name)
		}
		return nil
	}
	if a.BurstCeilingPct <= 0 || a.BurstCeilingPct > 1 {
		return fmt.Errorf("account %q: burst_ceiling_pct must be in (0, 1], got %g", a.Name, a.BurstCeilingPct)
	}
	if a.BurstDrawCap < 0 {
		return fmt.Errorf("account %q: burst_draw_cap must be >= 0", a.Name)
	}
	return nil
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
		if err := a.validateBurst(); err != nil {
			return err
		}
	}
	// Node-type cost: each rate must be a whole number of units/second; each
	// partition's node types must resolve to a configured node type.
	for name, nr := range c.NodeTypes {
		if _, err := nr.perSecond(); err != nil {
			return fmt.Errorf("node_type %q: %w", name, err)
		}
	}
	pseen := make(map[string]bool, len(c.Partitions))
	for _, p := range c.Partitions {
		if p.Name == "" {
			return fmt.Errorf("partition with empty name")
		}
		if pseen[p.Name] {
			return fmt.Errorf("duplicate partition %q", p.Name)
		}
		pseen[p.Name] = true
		if len(p.NodeTypes) == 0 {
			return fmt.Errorf("partition %q: no node types", p.Name)
		}
		for _, nt := range p.NodeTypes {
			if _, ok := c.NodeTypes[nt]; !ok {
				return fmt.Errorf("partition %q: unknown node type %q", p.Name, nt)
			}
		}
	}
	return nil
}
