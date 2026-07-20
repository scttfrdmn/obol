// Package shim models the Slurm-side gate client: the logic that runs inside the
// job_submit hook, calls obold, and — critically — decides what to do when obold
// is slow or down. That fail-mode decision is the part that must be correct with
// no daemon reachable, so it is pure Go modeled and tested here, independent of
// the actual Lua that will call it (docs/SEAM_DESIGN.md §6 and §7).
//
// The controller-lock constraint (§1) forces two rules this package encodes:
//
//   - A hard timeout: "slow" and "down" are the same to slurmctld, so a daemon
//     that does not answer within the timeout is treated as down. A healthy
//     daemon answers in microseconds; the timeout only fires on real trouble.
//   - The open/closed decision comes from a LOCAL static table, never a
//     round-trip — because the daemon being unreachable is the whole scenario.
package shim

import (
	"context"
	"errors"
	"time"

	"github.com/scttfrdmn/obol/internal/wire"
)

// PartitionClass is the owned-vs-rented axis that drives fail mode (§6). It is
// the single property three per-partition booleans track; for the fail decision
// only fail_closed matters, and it follows directly from the class.
type PartitionClass int

const (
	// OnPrem is owned hardware: idle capacity is free, so never block users for a
	// daemon hiccup — fail OPEN.
	OnPrem PartitionClass = iota
	// Cloud is rented capacity costing real money: an unfunded job must never slip
	// through while the gate is down — fail CLOSED.
	Cloud
)

// failClosed reports the static fail-closed policy for a class. Cloud fails
// closed; on-prem fails open. This is the local table's core mapping.
func (c PartitionClass) failClosed() bool { return c == Cloud }

// Decision is the gate verdict the shim returns to Slurm.
type Decision int

const (
	// Admit lets the job through (SUCCESS from job_submit).
	Admit Decision = iota
	// Reject blocks the job (job_submit sets err_msg and returns error).
	Reject
)

func (d Decision) String() string {
	if d == Admit {
		return "admit"
	}
	return "reject"
}

// Policy is the shim's local configuration: the per-partition class table and
// the hard timeout. It is consulted with no daemon round-trip.
type Policy struct {
	// Classes maps partition name -> class. A partition absent from the table is
	// treated per DefaultClass.
	Classes map[string]PartitionClass
	// DefaultClass applies to partitions not in Classes. Default zero value is
	// OnPrem (fail open) — the safe default for an unconfigured on-prem site; a
	// cloud site sets this to Cloud explicitly.
	DefaultClass PartitionClass
	// Timeout bounds a single GATE call. Zero means a small built-in default.
	Timeout time.Duration
}

func (p Policy) classOf(partition string) PartitionClass {
	if c, ok := p.Classes[partition]; ok {
		return c
	}
	return p.DefaultClass
}

func (p Policy) timeout() time.Duration {
	if p.Timeout <= 0 {
		return 50 * time.Millisecond
	}
	return p.Timeout
}

// GateFunc performs one GATE round-trip to the daemon. It is injected so tests
// can simulate a healthy, slow, or down daemon without a real socket. It must
// honor ctx cancellation — that is how the hard timeout is enforced.
type GateFunc func(ctx context.Context, req *wire.GateRequest) (*wire.GateResponse, error)

// Client is the shim-side gate. It calls the daemon with a hard timeout and, on
// any failure to get a verdict, falls back to the local per-partition policy.
type Client struct {
	policy Policy
	gate   GateFunc
}

// NewClient builds a Client with a local policy and a GATE transport.
func NewClient(policy Policy, gate GateFunc) *Client {
	return &Client{policy: policy, gate: gate}
}

// ErrFailClosed is the reason surfaced when a job is rejected because the daemon
// was unreachable on a fail-closed (cloud) partition.
var ErrFailClosed = errors.New("budget daemon unreachable; partition fails closed")

// Gate returns the admit/reject decision for a submission. On a clean daemon
// answer it honors that answer. On timeout or transport error it applies the
// local fail-mode policy for the partition: cloud rejects, on-prem admits.
//
// The returned reason is non-empty on Reject, suitable for job_submit's err_msg.
func (c *Client) Gate(req *wire.GateRequest) (Decision, string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.policy.timeout())
	defer cancel()

	resp, err := c.gate(ctx, req)
	if err != nil {
		// Slow (deadline exceeded) and down (dial/read error) are the same here:
		// no verdict, so fall back to the local table.
		return c.failMode(req.Partition)
	}
	if resp.Allow {
		return Admit, ""
	}
	// A clean rejection from the daemon (insufficient budget, etc.) is honored
	// regardless of partition class — the gate answered.
	return Reject, resp.Reason
}

// failMode applies the local static policy when the daemon gave no verdict.
func (c *Client) failMode(partition string) (Decision, string) {
	if c.policy.classOf(partition).failClosed() {
		return Reject, ErrFailClosed.Error()
	}
	return Admit, ""
}
