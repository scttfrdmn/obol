package cli

import (
	"flag"
	"fmt"
	"io"
	"math"
	"strings"

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
	account := fs.String("account", "", "Slurm account")
	partition := fs.String("partition", "", "partition")
	timeLimit := fs.Int64("time-limit", 0, "requested walltime, seconds")
	ntasks := fs.Int("ntasks", 1, "task count (>1 = array)")
	uid := fs.Uint64("uid", 0, "submitting user id")
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
		TimeLimit: *timeLimit, NTasks: *ntasks,
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" || *jobid == "" {
		return fail(errOut, fmt.Errorf("--token and --jobid are required"))
	}
	resp, err := roundTrip(*socket, wire.BindFrame(&wire.BindRequest{Token: *token, JobID: *jobid}))
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
