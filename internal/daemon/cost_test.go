package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

func TestWeightsRate(t *testing.T) {
	cases := []struct {
		name string
		w    Weights
		tres wire.TRES
		want budget.Units
	}{
		{"flat (no weights)", Weights{}, wire.TRES{CPUs: 8, GPUs: 2}, 0},
		{"cpu only", Weights{PerCPU: 1}, wire.TRES{CPUs: 8}, 8},
		{"gpu heavy", Weights{PerCPU: 1, PerGPU: 100}, wire.TRES{CPUs: 8, GPUs: 2}, 8 + 200},
		{"mem", Weights{PerMem: 2}, wire.TRES{Mem: 1000}, 2000},
		{"all", Weights{PerCPU: 1, PerGPU: 50, PerMem: 1}, wire.TRES{CPUs: 4, GPUs: 1, Mem: 500}, 4 + 50 + 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Rate(tc.tres); got != tc.want {
				t.Errorf("Rate(%+v) = %d, want %d", tc.tres, got, tc.want)
			}
		})
	}
}

func TestWeightsZero(t *testing.T) {
	if !(Weights{}).Zero() {
		t.Error("empty Weights should be Zero")
	}
	if (Weights{PerGPU: 1}).Zero() {
		t.Error("Weights with a set field should not be Zero")
	}
}
