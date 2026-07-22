package cli

import (
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/scttfrdmn/obol/internal/wire"
)

// cmdShow renders a consistent snapshot of the budget: balance, burn rate,
// time-to-empty, live work, burst, and the conservation check.
func cmdShow(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account to show (omit if only one is configured)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resp, err := roundTrip(*socket, wire.StatusFrame(*account))
	if err != nil {
		return fail(errOut, err)
	}
	s := resp.StatusResp
	if s == nil {
		return fail(errOut, fmt.Errorf("empty status response"))
	}
	if !s.OK {
		return fail(errOut, fmt.Errorf("%s", s.Reason))
	}
	pf(out, "Account:       %s\n", s.Account)
	pf(out, "Balance:       %d / %d\n", s.B, s.B0)
	pf(out, "Reserved:      %d\n", s.Reserved)
	pf(out, "Consumed:      %d\n", s.Consumed)
	pf(out, "Write-off:     %d\n", s.WriteOff)
	pf(out, "Cost rate:     %d/s\n", s.C)
	pf(out, "Time-to-empty: %s\n", fmtTTE(s.TimeToEmpty))
	pf(out, "Window:        [%d, %d)%s\n", s.TS, s.TE, lapsedTag(s.Lapsed))
	pf(out, "Live:          %d escrows, %d arrays\n", s.LiveEscrows, s.LiveArrays)
	if s.BurstEnabled {
		pf(out, "Burst:         pot %d / ceiling %d, rLive %d\n", s.BurstPot, s.BurstCeiling, s.RLive)
	} else {
		pl(out, "Burst:         disabled")
	}
	pf(out, "Conservation:  %s (sum %d, B0 %d)\n", okTag(s.ConservationOK), s.ConservationSum, s.B0)
	if !s.ConservationOK {
		return fail(errOut, fmt.Errorf("conservation VIOLATED"))
	}
	return 0
}

// cmdGate issues a GATE and prints the verdict. Exit code 3 marks a clean
// rejection (distinct from a transport error, exit 1), so scripts can tell
// "daemon said no" from "daemon unreachable".
func cmdGate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "Slurm account (single-source)")
	partition := fs.String("partition", "", "partition")
	timeLimit := fs.Int64("time-limit", 0, "requested walltime, seconds")
	ntasks := fs.Int("ntasks", 1, "task count (>1 = array)")
	uid := fs.Uint64("uid", 0, "submitting user id")
	var sources stringList
	fs.Var(&sources, "source", "funding source account, ordered fallback (repeatable; overrides --account for multi-source, #54)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timeLimit <= 0 {
		return fail(errOut, fmt.Errorf("--time-limit must be > 0"))
	}
	if *uid > math.MaxUint32 {
		return fail(errOut, fmt.Errorf("--uid out of range"))
	}
	resp, err := roundTrip(*socket, wire.GateFrame(&wire.GateRequest{
		Account: *account, Partition: *partition, UID: uint32(*uid),
		TimeLimit: *timeLimit, NTasks: *ntasks, Sources: sources,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	g := resp.GateResp
	if g == nil {
		return fail(errOut, fmt.Errorf("empty gate response"))
	}
	if !g.Allow {
		pf(out, "reject: %s\n", g.Reason)
		return 3
	}
	pf(out, "allow %s\n", g.Token)
	return 0
}

// cmdBind binds a minted token to a Slurm job id (also fires the start event).
func cmdBind(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("bind", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	token := fs.String("token", "", "gate token")
	jobid := fs.String("jobid", "", "Slurm job id")
	nodeType := fs.String("node-type", "", "actual node type (triggers the cost true-up)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" || *jobid == "" {
		return fail(errOut, fmt.Errorf("--token and --jobid are required"))
	}
	resp, err := roundTrip(*socket, wire.BindNodeFrame(*token, *jobid, *nodeType))
	if err != nil {
		return fail(errOut, err)
	}
	if resp.BindResp == nil || !resp.BindResp.OK {
		return fail(errOut, fmt.Errorf("bind rejected: %s", reasonOf(resp)))
	}
	pl(out, "ok")
	return 0
}

// cmdSettle closes out a job by kind. Exactly one of --jobid/--token identifies
// the escrow.
func cmdSettle(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("settle", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	jobid := fs.String("jobid", "", "Slurm job id")
	token := fs.String("token", "", "gate token")
	kind := fs.String("kind", "", "complete|timeout|cancel|infrafail")
	runtime := fs.Int64("runtime", 0, "runtime seconds (complete)")
	elapsed := fs.Int64("elapsed", 0, "elapsed seconds (cancel/infrafail)")
	ifPresent := fs.Bool("if-present", false, "treat an unknown/already-settled job as a no-op success (for jobcomp/epilog hooks that may double-fire)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jobid == "" && *token == "" {
		return fail(errOut, fmt.Errorf("one of --jobid or --token is required"))
	}
	sk, err := parseSettleKind(*kind)
	if err != nil {
		return fail(errOut, err)
	}
	resp, err := roundTrip(*socket, wire.SettleFrame(&wire.SettleRequest{
		JobID: *jobid, Token: *token, Kind: sk, Runtime: *runtime, Elapsed: *elapsed,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	if resp.SettleResp == nil || !resp.SettleResp.OK {
		reason := reasonOf(resp)
		// A settle for a job with no live escrow means it was already settled (or
		// never gated). With --if-present that is the expected, benign case for a
		// completion hook that may fire after another path already settled.
		if *ifPresent && isAlreadySettled(reason) {
			pl(out, "already settled")
			return 0
		}
		return fail(errOut, fmt.Errorf("settle rejected: %s", reason))
	}
	pl(out, "ok")
	return 0
}

// cmdTopUp adds money to an account's budget (admin-only, enforced daemon-side
// by peer credentials).
func cmdTopUp(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("topup", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account to top up")
	amount := fs.Int64("amount", 0, "amount to add (positive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || *amount <= 0 {
		return fail(errOut, fmt.Errorf("--account and a positive --amount are required"))
	}
	resp, err := roundTrip(*socket, wire.TopUpFrame(*account, *amount))
	if err != nil {
		return fail(errOut, err)
	}
	if resp.TopUpResp == nil || !resp.TopUpResp.OK {
		r := "empty response"
		if resp.TopUpResp != nil {
			r = resp.TopUpResp.Reason
		}
		return fail(errOut, fmt.Errorf("topup rejected: %s", r))
	}
	pf(out, "ok: %s balance %d (allocation %d)\n", *account, resp.TopUpResp.NewBalance, resp.TopUpResp.NewB0)
	return 0
}

// cmdList prints the accounts the caller may see and their balances.
func cmdList(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resp, err := roundTrip(*socket, wire.ListFrame())
	if err != nil {
		return fail(errOut, err)
	}
	if resp.ListResp == nil || !resp.ListResp.OK {
		return fail(errOut, fmt.Errorf("list failed"))
	}
	pf(out, "%-20s %12s %12s %8s %s\n", "ACCOUNT", "BALANCE", "ALLOCATION", "LIVE", "STATUS")
	for _, a := range resp.ListResp.Accounts {
		status := "active"
		if a.Lapsed {
			status = "lapsed"
		}
		pf(out, "%-20s %12d %12d %8d %s\n", a.Account, a.B, a.B0, a.Live, status)
	}
	return 0
}

// cmdLog renders an account's transaction/audit log (the WAL), time-ordered.
func cmdLog(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account to render (omit if only one is configured)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resp, err := roundTrip(*socket, wire.LogFrame(*account))
	if err != nil {
		return fail(errOut, err)
	}
	if resp.LogResp == nil || !resp.LogResp.OK {
		r := "empty response"
		if resp.LogResp != nil {
			r = resp.LogResp.Reason
		}
		return fail(errOut, fmt.Errorf("log failed: %s", r))
	}
	pf(out, "# transaction log for account %s (%d entries)\n", resp.LogResp.Account, len(resp.LogResp.Entries))
	for _, e := range resp.LogResp.Entries {
		pf(out, "t=%-8d %-22s %s\n", e.Now, e.Kind, logDetail(e))
	}
	return 0
}

// logDetail renders the fields relevant to a log entry's kind.
func logDetail(e wire.LogEntry) string {
	switch {
	case e.Kind == "topup" || e.Kind == "withdraw":
		d := fmt.Sprintf("amount=%d", e.Amount)
		if e.Xfer != "" {
			d += " xfer=" + e.Xfer
		}
		return d
	case e.Kind == "set-rate":
		return fmt.Sprintf("rate=%d", e.Rate)
	case e.Kind == "set-window":
		return fmt.Sprintf("window=[%d,%d)", e.TS, e.TE)
	case e.ArrayID != "":
		d := "array=" + e.ArrayID
		if e.N > 0 {
			d += fmt.Sprintf(" n=%d", e.N)
		}
		if e.Idx != 0 || e.Kind == "start-task" {
			d += fmt.Sprintf(" idx=%d", e.Idx)
		}
		return d + logCostDetail(e)
	case e.JobID != "":
		return "job=" + e.JobID + logCostDetail(e)
	default:
		return ""
	}
}

func logCostDetail(e wire.LogEntry) string {
	d := ""
	if e.Rate != 0 {
		d += fmt.Sprintf(" rate=%d", e.Rate)
	}
	if e.W != 0 {
		d += fmt.Sprintf(" w=%d", e.W)
	}
	if e.Runtime != 0 {
		d += fmt.Sprintf(" runtime=%d", e.Runtime)
	}
	if e.Elapsed != 0 {
		d += fmt.Sprintf(" elapsed=%d", e.Elapsed)
	}
	return d
}

// cmdSetRate changes an account's flat cost rate (admin-gated daemon-side).
func cmdSetRate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("set-rate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account")
	rate := fs.Int64("rate", 0, "new flat cost rate (units/second, positive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || *rate <= 0 {
		return fail(errOut, fmt.Errorf("--account and a positive --rate are required"))
	}
	resp, err := roundTrip(*socket, wire.SetRateFrame(*account, *rate))
	if err != nil {
		return fail(errOut, err)
	}
	return ackResult(out, errOut, resp, fmt.Sprintf("rate for %s set to %d/s", *account, *rate))
}

// cmdSetWindow changes an account's time window. Accepts either --window <dur>
// (sets [now, now+dur)) or explicit --start/--end (RFC3339 or epoch seconds).
func cmdSetWindow(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("set-window", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account")
	window := fs.Duration("window", 0, "window length from now (e.g. 720h); alternative to --start/--end")
	start := fs.String("start", "", "window start (RFC3339 or epoch seconds)")
	end := fs.String("end", "", "window end (RFC3339 or epoch seconds)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" {
		return fail(errOut, fmt.Errorf("--account is required"))
	}
	var ts, te int64
	switch {
	case *window > 0:
		ts = time.Now().Unix()
		te = ts + int64(window.Seconds())
	case *start != "" && *end != "":
		var err error
		if ts, err = parseTime(*start); err != nil {
			return fail(errOut, fmt.Errorf("--start: %w", err))
		}
		if te, err = parseTime(*end); err != nil {
			return fail(errOut, fmt.Errorf("--end: %w", err))
		}
	default:
		return fail(errOut, fmt.Errorf("provide --window, or both --start and --end"))
	}
	resp, err := roundTrip(*socket, wire.SetWindowFrame(*account, ts, te))
	if err != nil {
		return fail(errOut, err)
	}
	return ackResult(out, errOut, resp, fmt.Sprintf("window for %s set to [%d, %d)", *account, ts, te))
}

// ackResult renders a generic ack response.
func ackResult(out, errOut io.Writer, resp *wire.Frame, okMsg string) int {
	if resp.AckResp == nil || !resp.AckResp.OK {
		r := "empty response"
		if resp.AckResp != nil {
			r = resp.AckResp.Reason
		}
		return fail(errOut, fmt.Errorf("rejected: %s", r))
	}
	pf(out, "ok: %s\n", okMsg)
	return 0
}

// parseTime accepts RFC3339 or a bare epoch-seconds integer.
func parseTime(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("want RFC3339 or epoch seconds, got %q", s)
	}
	return t.Unix(), nil
}

// cmdResolve dry-runs the gate decision for a submission: which budget resolves,
// the effective rate, access, and whether it would be admitted — no escrow.
func cmdResolve(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account (the resolution key)")
	partition := fs.String("partition", "", "partition (for node-type pricing)")
	timeLimit := fs.Int64("time-limit", 0, "walltime seconds (optional: also checks funding)")
	uid := fs.Uint64("uid", 0, "submitter uid (optional: also checks access)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" {
		return fail(errOut, fmt.Errorf("--account is required"))
	}
	if *uid > math.MaxUint32 {
		return fail(errOut, fmt.Errorf("--uid out of range"))
	}
	resp, err := roundTrip(*socket, wire.ResolveFrame(&wire.ResolveRequest{
		Account: *account, Partition: *partition, UID: uint32(*uid), TimeLimit: *timeLimit,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	r := resp.ResolveResp
	if r == nil {
		return fail(errOut, fmt.Errorf("empty resolve response"))
	}
	if !r.OK {
		return fail(errOut, fmt.Errorf("%s", r.Reason))
	}
	pf(out, "Account:    %s\n", r.Account)
	pf(out, "Resolved:   %v\n", r.Resolved)
	if r.Resolved {
		pf(out, "Rate:       %d/s (%s)\n", r.Rate, r.RateSource)
		pf(out, "Balance:    %d\n", r.Balance)
		if r.Cost > 0 {
			pf(out, "Cost:       %d\n", r.Cost)
		}
		pf(out, "Authorized: %v\n", r.Authorized)
	}
	pf(out, "Admits:     %v\n", r.Admits)
	pf(out, "Why:        %s\n", r.Decision)
	// Exit 3 = would be rejected (distinct from transport error 1), mirroring gate.
	if !r.Admits {
		return 3
	}
	return 0
}

// cmdSimulate reports whether a hypothetical job would fund now, its cost, and
// the budget's projected runway — committing nothing.
func cmdSimulate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account")
	partition := fs.String("partition", "", "partition (for node-type pricing)")
	timeLimit := fs.Int64("time-limit", 0, "walltime seconds")
	cpus := fs.Int64("cpus", 0, "requested CPUs (for TRES pricing)")
	gpus := fs.Int64("gpus", 0, "requested GPUs")
	mem := fs.Int64("mem", 0, "requested memory MB")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || *timeLimit <= 0 {
		return fail(errOut, fmt.Errorf("--account and a positive --time-limit are required"))
	}
	resp, err := roundTrip(*socket, wire.SimulateFrame(&wire.SimulateRequest{
		Account: *account, Partition: *partition, TimeLimit: *timeLimit,
		TRES: wire.TRES{CPUs: *cpus, GPUs: *gpus, Mem: *mem},
	}))
	if err != nil {
		return fail(errOut, err)
	}
	r := resp.SimulateResp
	if r == nil {
		return fail(errOut, fmt.Errorf("empty simulate response"))
	}
	if !r.OK {
		return fail(errOut, fmt.Errorf("%s", r.Reason))
	}
	pf(out, "Account:  %s\n", r.Account)
	pf(out, "Rate:     %d/s (%s)\n", r.Rate, r.RateSource)
	pf(out, "Cost:     %d\n", r.Cost)
	pf(out, "Balance:  %d\n", r.Balance)
	pf(out, "Runway:   %s\n", fmtTTE(r.Runway))
	if r.Admit {
		pf(out, "Verdict:  WOULD FUND\n")
		return 0
	}
	pf(out, "Verdict:  WOULD NOT FUND (%s)\n", r.Deny)
	return 3
}

// cmdDispatch asks whether a pending job may start now given burst headroom, or
// must hold at priority 0 — the CLI face of the site_factor dispatch query.
// Exit 0 = would dispatch, 3 = would hold (mirrors gate/simulate), 1 transport.
func cmdDispatch(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account")
	partition := fs.String("partition", "", "partition (for node-type pricing)")
	timeLimit := fs.Int64("time-limit", 0, "walltime seconds")
	cpus := fs.Int64("cpus", 0, "requested CPUs (for TRES pricing)")
	gpus := fs.Int64("gpus", 0, "requested GPUs")
	mem := fs.Int64("mem", 0, "requested memory MB")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || *timeLimit <= 0 {
		return fail(errOut, fmt.Errorf("--account and a positive --time-limit are required"))
	}
	resp, err := roundTrip(*socket, wire.DispatchFrame(&wire.DispatchRequest{
		Account: *account, Partition: *partition, TimeLimit: *timeLimit,
		TRES: wire.TRES{CPUs: *cpus, GPUs: *gpus, Mem: *mem},
	}))
	if err != nil {
		return fail(errOut, err)
	}
	r := resp.DispatchResp
	if r == nil {
		return fail(errOut, fmt.Errorf("empty dispatch response"))
	}
	if !r.OK {
		return fail(errOut, fmt.Errorf("%s", r.Reason))
	}
	pf(out, "Account:  %s\n", r.Account)
	pf(out, "Rate:     %d/s (%s)\n", r.Rate, r.RateSource)
	pf(out, "Reserve:  %d tokens\n", r.Reserve)
	pf(out, "Pot:      %d\n", r.Pot)
	if r.Dispatch {
		pf(out, "Verdict:  WOULD DISPATCH\n")
		return 0
	}
	pf(out, "Verdict:  WOULD HOLD (%s)\n", r.Hold)
	return 3
}

// stringList is a flag.Value that accumulates repeated flags (e.g. --user a
// --user b -> ["a","b"]).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cmdCreate creates a new account budget at runtime (admin-gated daemon-side).
func cmdCreate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account name")
	balance := fs.Int64("balance", 0, "initial allocation (non-negative)")
	rate := fs.Int64("rate", 0, "flat cost rate (units/second, positive)")
	window := fs.String("window", "", "budget window (Go duration, e.g. 720h; default 720h)")
	var users, groups stringList
	fs.Var(&users, "allow-user", "restrict access to this user (repeatable)")
	fs.Var(&groups, "allow-group", "restrict access to this group (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || *rate <= 0 || *balance < 0 {
		return fail(errOut, fmt.Errorf("--account, a positive --rate, and a non-negative --balance are required"))
	}
	resp, err := roundTrip(*socket, wire.CreateFrame(&wire.CreateRequest{
		Account: *account, Balance: *balance, Rate: *rate, Window: *window,
		AllowUsers: users, AllowGroups: groups,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	return ackResult(out, errOut, resp, fmt.Sprintf("account %s created (balance %d, rate %d/s)", *account, *balance, *rate))
}

// cmdAttach adds (detach=false) or removes (detach=true) users/groups on an
// account's access list.
func cmdAttach(args []string, out, errOut io.Writer, detach bool) int {
	name := "attach"
	if detach {
		name = "detach"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	account := fs.String("account", "", "account")
	var users, groups stringList
	fs.Var(&users, "user", "user to "+name+" (repeatable)")
	fs.Var(&groups, "group", "group to "+name+" (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account == "" || (len(users) == 0 && len(groups) == 0) {
		return fail(errOut, fmt.Errorf("--account and at least one --user or --group are required"))
	}
	resp, err := roundTrip(*socket, wire.AttachFrame(&wire.AttachRequest{
		Account: *account, Users: users, Groups: groups, Detach: detach,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	r := resp.AttachResp
	if r == nil || !r.OK {
		reason := "empty response"
		if r != nil {
			reason = r.Reason
		}
		return fail(errOut, fmt.Errorf("%s rejected: %s", name, reason))
	}
	allow := "open (no restriction)"
	if len(r.AllowUsers) > 0 || len(r.AllowGroups) > 0 {
		allow = fmt.Sprintf("users=%v groups=%v", r.AllowUsers, r.AllowGroups)
	}
	pf(out, "ok: %s access for %s → %s\n", name, *account, allow)
	return 0
}

// cmdTransfer moves money from one account to another (admin-gated daemon-side).
// Exactly one of --amount or --all selects how much.
func cmdTransfer(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("transfer", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	from := fs.String("from", "", "source account")
	to := fs.String("to", "", "destination account")
	amount := fs.Int64("amount", 0, "amount to move (positive); mutually exclusive with --all")
	all := fs.Bool("all", false, "move the source's entire available balance")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *to == "" {
		return fail(errOut, fmt.Errorf("--from and --to are required"))
	}
	if *all == (*amount > 0) { // neither, or both
		return fail(errOut, fmt.Errorf("provide exactly one of --amount (positive) or --all"))
	}
	resp, err := roundTrip(*socket, wire.TransferFrame(&wire.TransferRequest{
		From: *from, To: *to, Amount: *amount, All: *all,
	}))
	if err != nil {
		return fail(errOut, err)
	}
	r := resp.TransferResp
	if r == nil || !r.OK {
		reason := "empty response"
		if r != nil {
			reason = r.Reason
		}
		pf(out, "reject: %s\n", reason)
		return 3
	}
	pf(out, "moved %d: %s → %s (%s now %d, %s now %d)\n",
		r.Moved, *from, *to, *from, r.FromBalance, *to, r.ToBalance)
	return 0
}

// --- small formatting/parsing helpers ---

func parseSettleKind(s string) (wire.SettleKind, error) {
	switch s {
	case "complete":
		return wire.SettleComplete, nil
	case "timeout":
		return wire.SettleTimeout, nil
	case "cancel":
		return wire.SettleCancel, nil
	case "infrafail":
		return wire.SettleInfraFail, nil
	default:
		return "", fmt.Errorf("--kind must be complete|timeout|cancel|infrafail, got %q", s)
	}
}

// isAlreadySettled reports whether a settle-rejection reason means "no live
// escrow for this job" — i.e. it was already settled or never gated. Matches the
// kernel's ErrNoSuchJob ("no such escrow") and the daemon's unbound-jobid text.
func isAlreadySettled(reason string) bool {
	return strings.Contains(reason, "no such escrow") ||
		strings.Contains(reason, "unknown job")
}

func reasonOf(resp *wire.Frame) string {
	if resp.BindResp != nil {
		return resp.BindResp.Reason
	}
	if resp.SettleResp != nil {
		return resp.SettleResp.Reason
	}
	return "unknown"
}

func fmtTTE(secs int64) string {
	if secs < 0 {
		return "never (no burn)"
	}
	return fmt.Sprintf("%ds", secs)
}

func lapsedTag(lapsed bool) string {
	if lapsed {
		return " LAPSED"
	}
	return ""
}

func okTag(ok bool) string {
	if ok {
		return "OK"
	}
	return "VIOLATED"
}
