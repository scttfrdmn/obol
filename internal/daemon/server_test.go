package daemon

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// testClock is a deterministic, monotone clock the test drives explicitly so the
// session is reproducible (the kernel is a pure function of now).
type testClock struct {
	mu sync.Mutex
	t  budget.Seconds
}

func (c *testClock) now() budget.Seconds {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) set(t budget.Seconds) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// newTestServer builds a durable budget in a temp dir and serves it over a Unix
// socket, returning a dial func, the clock, and the budget for conservation
// assertions.
func newTestServer(t *testing.T) (func() net.Conn, *testClock, *budget.Budget) {
	t.Helper()
	dir := t.TempDir()
	// c=1 unit/sec, B0=100000, window [0, 100000).
	bd, err := budget.NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatalf("NewDurable: %v", err)
	}
	clk := &testClock{}
	srv := New(bd, clk.now)

	ln, err := net.Listen("unix", filepath.Join(dir, "obold.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = bd.Close()
	})

	// dial retries a transient ECONNREFUSED: under a thundering herd of
	// concurrent dials the listen backlog can briefly fill, which a real client
	// handles by retrying. This is a test-harness concern, not server behavior.
	dial := func() net.Conn {
		for attempt := 0; ; attempt++ {
			conn, derr := net.Dial("unix", ln.Addr().String())
			if derr == nil {
				return conn
			}
			if attempt >= 100 {
				t.Fatalf("dial: %v", derr)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return dial, clk, bd
}

// call writes one request and reads one response on a fresh connection (the
// hot-path one-shot pattern the shim uses).
func call(t *testing.T, dial func() net.Conn, req *wire.Frame) *wire.Frame {
	t.Helper()
	conn := dial()
	defer func() { _ = conn.Close() }()
	if err := wire.WriteFrame(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := wire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp
}

// TestGateBindSettleLifecycle drives a full single-job lifecycle through the
// socket and asserts conservation holds at the end.
func TestGateBindSettleLifecycle(t *testing.T) {
	dial, clk, bd := newTestServer(t)

	// GATE a 1000-second job at c=1 -> escrow 1000.
	clk.set(10)
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{
		Account: "lab", Partition: "cloud", TimeLimit: 1000, NTasks: 1,
	}))
	if resp.GateResp == nil || !resp.GateResp.Allow {
		t.Fatalf("gate rejected: %+v", resp.GateResp)
	}
	token := resp.GateResp.Token
	if bal := bd.Balance(); bal != 99000 {
		t.Errorf("after gate: balance = %d, want 99000", bal)
	}

	// BIND token -> jobid 4711 (fires Start).
	bresp := call(t, dial, wire.BindFrame(&wire.BindRequest{Token: token, JobID: "4711"}))
	if bresp.BindResp == nil || !bresp.BindResp.OK {
		t.Fatalf("bind failed: %+v", bresp.BindResp)
	}

	// SETTLE complete after 400s runtime -> bill 400, refund 600.
	clk.set(420)
	sresp := call(t, dial, wire.SettleFrame(&wire.SettleRequest{
		JobID: "4711", Kind: wire.SettleComplete, Runtime: 400,
	}))
	if sresp.SettleResp == nil || !sresp.SettleResp.OK {
		t.Fatalf("settle failed: %+v", sresp.SettleResp)
	}
	if bal := bd.Balance(); bal != 99600 {
		t.Errorf("after settle: balance = %d, want 99600", bal)
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation violated: got %d, want %d", sum, 100000)
	}
}

// TestGateRejectInsufficient confirms an unfundable job is rejected at the gate
// and nothing is escrowed.
func TestGateRejectInsufficient(t *testing.T) {
	dial, clk, bd := newTestServer(t)
	clk.set(1)
	// Request 200000 seconds at c=1 -> cost 200000 > B0 100000.
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{
		Account: "lab", Partition: "cloud", TimeLimit: 200000, NTasks: 1,
	}))
	if resp.GateResp == nil || resp.GateResp.Allow {
		t.Fatalf("expected rejection, got %+v", resp.GateResp)
	}
	if bal := bd.Balance(); bal != 100000 {
		t.Errorf("balance changed on reject: %d, want 100000", bal)
	}
}

// TestArrayLifecycle drives an array GATE and confirms the all-or-nothing debit.
func TestArrayLifecycle(t *testing.T) {
	dial, clk, bd := newTestServer(t)
	clk.set(5)
	// 10 tasks * 100s * c=1 = 1000 debited up front.
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{
		Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 10,
	}))
	if resp.GateResp == nil || !resp.GateResp.Allow {
		t.Fatalf("array gate rejected: %+v", resp.GateResp)
	}
	if bal := bd.Balance(); bal != 99000 {
		t.Errorf("after array gate: balance = %d, want 99000", bal)
	}
	if ok, sum := bd.ConservationOKArrays(); !ok {
		t.Errorf("conservation violated: got %d", sum)
	}
}

// TestPing confirms the health check round-trips.
func TestPing(t *testing.T) {
	dial, _, _ := newTestServer(t)
	resp := call(t, dial, wire.PingFrame())
	if resp.MsgKind != wire.KindPing {
		t.Errorf("ping: kind = %q, want %q", resp.MsgKind, wire.KindPing)
	}
}

// TestConcurrentGates hammers the gate from many goroutines and asserts
// conservation survives. Run under -race to catch a broken lock discipline.
func TestConcurrentGates(t *testing.T) {
	dial, clk, bd := newTestServer(t)
	clk.set(1)
	const n = 200
	var wg sync.WaitGroup
	allowed := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each job costs 1000; B0=100000 funds ~100 of them.
			resp := call(t, dial, wire.GateFrame(&wire.GateRequest{
				Account: "lab", Partition: "cloud", TimeLimit: 1000, NTasks: 1,
			}))
			allowed[i] = resp.GateResp != nil && resp.GateResp.Allow
		}(i)
	}
	wg.Wait()

	got := 0
	for _, a := range allowed {
		if a {
			got++
		}
	}
	if got != 100 {
		t.Errorf("funded gates = %d, want exactly 100 (B0/cost)", got)
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation violated after storm: got %d, want 100000", sum)
	}
}

// TestCleanShutdown confirms closing the listener stops Serve without error.
func TestCleanShutdown(t *testing.T) {
	dir := t.TempDir()
	bd, err := budget.NewDurable(dir, 1, 1000, 0, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bd.Close() }()
	clk := &testClock{}
	srv := New(bd, clk.now)
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve returned %v on clean shutdown, want nil", err)
	}
}

// TestGateTRESWeightedCost confirms a gate with TRES escrows the weighted cost
// (rate = Σ tres×weight), not the budget's flat rate.
func TestGateTRESWeightedCost(t *testing.T) {
	dir := t.TempDir()
	bd, err := budget.NewDurable(dir, 1, 1_000_000, 0, 1_000_000, false) // flat C=1
	if err != nil {
		t.Fatal(err)
	}
	clk := &testClock{}
	clk.set(10)
	// GPU-heavy weights: 1/cpu-s + 100/gpu-s.
	srv := NewWithWeights(bd, clk.now, Weights{PerCPU: 1, PerGPU: 100})
	ln, err := net.Listen("unix", filepath.Join(dir, "obold.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = bd.Close() })

	dial := func() net.Conn {
		c, e := net.Dial("unix", ln.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		return c
	}

	// 8 CPUs + 2 GPUs => rate = 8 + 200 = 208; w=100 => cost 20800.
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{
		Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 1,
		TRES: wire.TRES{CPUs: 8, GPUs: 2},
	}))
	if resp.GateResp == nil || !resp.GateResp.Allow {
		t.Fatalf("gate rejected: %+v", resp.GateResp)
	}
	if bal := bd.Balance(); bal != 1_000_000-20800 {
		t.Errorf("weighted escrow wrong: balance = %d, want %d", bal, 1_000_000-20800)
	}
}
