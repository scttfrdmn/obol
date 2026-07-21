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
