package daemon

import (
	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// Weights maps a job's requested TRES to a per-second cost rate, mirroring
// Slurm's TRESBillingWeights (docs/SEAM_DESIGN.md §5). The rate the kernel bills
// is rate·walltime, so these are money-units per resource-second.
//
// All-zero weights mean "flat rate": Rate returns 0, and the kernel then falls
// back to the budget's own bd.C (SubmitAt with c<=0). This is the default, so a
// daemon started without -tres-* flags behaves exactly as before.
type Weights struct {
	PerCPU budget.Units // per allocated CPU-second
	PerGPU budget.Units // per allocated GPU-second
	PerMem budget.Units // per allocated MB-second
}

// Zero reports whether no weight is set (flat-rate mode).
func (w Weights) Zero() bool { return w.PerCPU == 0 && w.PerGPU == 0 && w.PerMem == 0 }

// Rate returns the per-second cost for a job requesting t. Returns 0 in
// flat-rate mode (no weights configured), which signals the kernel to use bd.C.
func (w Weights) Rate(t wire.TRES) budget.Units {
	if w.Zero() {
		return 0
	}
	return w.PerCPU*t.CPUs + w.PerGPU*t.GPUs + w.PerMem*t.Mem
}

// NodeCost resolves node-type-based rates (issue #65). Rates are stored as
// integer units/second (normalized from the config's h/m/s units at build time).
// worstRate returns the max rate over a partition's node types (the submit-time
// escrow estimate); rate returns a specific node type's rate (the dispatch-time
// true-up). Both return 0 when there is no node-type config for the input, which
// signals the caller to fall back to TRES/flat pricing.
type NodeCost struct {
	rates      map[string]budget.Units // node type -> units/second
	partitions map[string][]string     // partition -> node types it can place on
}

// BuildNodeCost normalizes a Config's node-type rates to per-second integers and
// indexes partitions. Returns an error if any rate does not divide cleanly.
func BuildNodeCost(cfg *Config) (*NodeCost, error) {
	nc := &NodeCost{
		rates:      make(map[string]budget.Units, len(cfg.NodeTypes)),
		partitions: make(map[string][]string, len(cfg.Partitions)),
	}
	for name, nr := range cfg.NodeTypes {
		ps, err := nr.perSecond()
		if err != nil {
			return nil, err
		}
		nc.rates[name] = ps
	}
	for _, p := range cfg.Partitions {
		nc.partitions[p.Name] = p.NodeTypes
	}
	return nc, nil
}

// enabled reports whether any node-type pricing is configured.
func (nc *NodeCost) enabled() bool { return nc != nil && len(nc.rates) > 0 }

// rate returns a node type's per-second rate, or 0 if unknown.
func (nc *NodeCost) rate(nodeType string) budget.Units {
	if nc == nil {
		return 0
	}
	return nc.rates[nodeType]
}

// worstRate returns the max rate over the partition's node types — the
// conservative submit-time escrow. Returns 0 if the partition has no node-type
// config (caller falls back to TRES/flat pricing).
func (nc *NodeCost) worstRate(partition string) budget.Units {
	if nc == nil {
		return 0
	}
	var worst budget.Units
	for _, nt := range nc.partitions[partition] {
		if r := nc.rates[nt]; r > worst {
			worst = r
		}
	}
	return worst
}
