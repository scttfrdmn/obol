package shim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scttfrdmn/obol/internal/wire"
)

// gate builders simulating the three daemon states the shim must handle.

// healthyGate answers immediately with the given verdict.
func healthyGate(allow bool, reason string) GateFunc {
	return func(_ context.Context, _ *wire.GateRequest) (*wire.GateResponse, error) {
		return &wire.GateResponse{Allow: allow, Reason: reason}, nil
	}
}

// downGate simulates an unreachable daemon: an immediate transport error.
func downGate() GateFunc {
	return func(_ context.Context, _ *wire.GateRequest) (*wire.GateResponse, error) {
		return nil, errors.New("dial unix: connection refused")
	}
}

// slowGate blocks past the deadline, then returns ctx.Err — modeling a hung
// daemon. The shim's timeout must fire and treat this as down.
func slowGate(delay time.Duration) GateFunc {
	return func(ctx context.Context, _ *wire.GateRequest) (*wire.GateResponse, error) {
		select {
		case <-time.After(delay):
			return &wire.GateResponse{Allow: true}, nil // would allow, but too late
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func cloudPolicy() Policy {
	return Policy{
		Classes: map[string]PartitionClass{"cloud-spot": Cloud, "onprem-owned": OnPrem},
		Timeout: 20 * time.Millisecond,
	}
}

func req(partition string) *wire.GateRequest {
	return &wire.GateRequest{Account: "lab", Partition: partition, TimeLimit: 100, NTasks: 1}
}

// TestHealthyDaemonHonored: a clean answer is honored on both partition classes,
// regardless of the local fail policy.
func TestHealthyDaemonHonored(t *testing.T) {
	cases := []struct {
		name      string
		allow     bool
		reason    string
		partition string
		want      Decision
	}{
		{"allow-cloud", true, "", "cloud-spot", Admit},
		{"allow-onprem", true, "", "onprem-owned", Admit},
		{"reject-cloud", false, "insufficient budget", "cloud-spot", Reject},
		{"reject-onprem", false, "insufficient budget", "onprem-owned", Reject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(cloudPolicy(), healthyGate(tc.allow, tc.reason))
			got, reason := c.Gate(req(tc.partition))
			if got != tc.want {
				t.Errorf("decision = %v, want %v", got, tc.want)
			}
			if tc.want == Reject && reason == "" {
				t.Error("reject with empty reason")
			}
		})
	}
}

// TestDaemonDownFailMode: with the daemon down, cloud fails closed (reject),
// on-prem fails open (admit).
func TestDaemonDownFailMode(t *testing.T) {
	c := NewClient(cloudPolicy(), downGate())

	if got, reason := c.Gate(req("cloud-spot")); got != Reject {
		t.Errorf("cloud down: decision = %v, want Reject", got)
	} else if reason == "" {
		t.Error("cloud down: expected a fail-closed reason")
	}

	if got, _ := c.Gate(req("onprem-owned")); got != Admit {
		t.Errorf("onprem down: decision = %v, want Admit", got)
	}
}

// TestSlowDaemonTreatedAsDown: a daemon that answers only after the timeout is
// treated exactly like a down daemon — cloud rejects, on-prem admits.
func TestSlowDaemonTreatedAsDown(t *testing.T) {
	// slowGate delay (100ms) >> policy timeout (20ms), so the deadline fires.
	c := NewClient(cloudPolicy(), slowGate(100*time.Millisecond))

	start := time.Now()
	got, _ := c.Gate(req("cloud-spot"))
	elapsed := time.Since(start)

	if got != Reject {
		t.Errorf("slow cloud: decision = %v, want Reject (fail closed)", got)
	}
	// The decision must come back around the timeout, not after the full delay —
	// proving the hard timeout, not the slow answer, drove it.
	if elapsed > 80*time.Millisecond {
		t.Errorf("slow cloud: took %v, expected ~timeout (20ms), not the full 100ms delay", elapsed)
	}

	if got, _ := c.Gate(req("onprem-owned")); got != Admit {
		t.Errorf("slow onprem: decision = %v, want Admit (fail open)", got)
	}
}

// TestTimeoutBoundary: a daemon answering just under the timeout is honored; one
// answering just over is treated as down. Brackets the boundary from both sides.
func TestTimeoutBoundary(t *testing.T) {
	const timeout = 60 * time.Millisecond
	pol := Policy{Classes: map[string]PartitionClass{"cloud-spot": Cloud}, Timeout: timeout}

	// Under the timeout: healthy answer honored (allow -> admit).
	fast := NewClient(pol, slowGate(timeout/3))
	if got, _ := fast.Gate(req("cloud-spot")); got != Admit {
		t.Errorf("under-timeout: decision = %v, want Admit (answer beat the deadline)", got)
	}

	// Over the timeout: treated as down -> cloud fails closed.
	slow := NewClient(pol, slowGate(timeout*3))
	if got, _ := slow.Gate(req("cloud-spot")); got != Reject {
		t.Errorf("over-timeout: decision = %v, want Reject (deadline fired)", got)
	}
}

// TestDefaultClassUnknownPartition: a partition not in the table uses
// DefaultClass. An unconfigured cloud site defaults to fail-closed.
func TestDefaultClassUnknownPartition(t *testing.T) {
	cloudDefault := NewClient(Policy{DefaultClass: Cloud, Timeout: 20 * time.Millisecond}, downGate())
	if got, _ := cloudDefault.Gate(req("never-heard-of-it")); got != Reject {
		t.Errorf("unknown partition, cloud default: decision = %v, want Reject", got)
	}

	onpremDefault := NewClient(Policy{DefaultClass: OnPrem, Timeout: 20 * time.Millisecond}, downGate())
	if got, _ := onpremDefault.Gate(req("never-heard-of-it")); got != Admit {
		t.Errorf("unknown partition, onprem default: decision = %v, want Admit", got)
	}
}
