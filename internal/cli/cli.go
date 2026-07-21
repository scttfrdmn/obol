package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/scttfrdmn/obol/internal/wire"
)

// Run is the CLI entry point. args is os.Args[1:]; out/errOut are injected so
// verbs are testable without capturing global stdout. Returns a process exit
// code (0 = success, nonzero = error or a rejected gate).
//
// The --socket global flag is accepted either before or after the verb; leading
// global flags are hoisted so the first non-flag token is the verb.
func Run(args []string, out, errOut io.Writer) int {
	args = hoistGlobalFlags(args)
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "show":
		return cmdShow(rest, out, errOut)
	case "gate":
		return cmdGate(rest, out, errOut)
	case "bind":
		return cmdBind(rest, out, errOut)
	case "settle":
		return cmdSettle(rest, out, errOut)
	case "topup":
		return cmdTopUp(rest, out, errOut)
	case "list":
		return cmdList(rest, out, errOut)
	case "ping":
		return cmdPing(rest, out, errOut)
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		pf(errOut, "obol: unknown command %q\n", verb)
		usage(errOut)
		return 2
	}
}

// hoistGlobalFlags moves a leading --socket flag (in either `--socket val` or
// `--socket=val` form) to after the verb, so `obol --socket X show` and
// `obol show --socket X` are equivalent. The verb's own flag set then parses it.
// Only leading global flags are moved; anything after the verb is left untouched.
func hoistGlobalFlags(args []string) []string {
	var leading []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--socket" || a == "-socket":
			if i+1 < len(args) {
				leading = append(leading, a, args[i+1])
				i += 2
				continue
			}
			i++ // dangling flag; let the verb parser report it
		case strings.HasPrefix(a, "--socket=") || strings.HasPrefix(a, "-socket="):
			leading = append(leading, a)
			i++
		default:
			// First non-global token is the verb: [verb] + [rest] + [hoisted globals].
			rest := append([]string{a}, args[i+1:]...)
			return append(rest, leading...)
		}
	}
	// Only global flags, no verb.
	return leading
}

func usage(w io.Writer) {
	pf(w, "%s", `obol — budget management CLI (talks to obold over its socket)

Usage:
  obol show                              show budget balance, burn, burst, conservation
  obol gate    --account A --partition P --time-limit S [--ntasks N]
  obol bind    --token T --jobid J
  obol settle  (--jobid J | --token T) --kind KIND [--runtime S] [--elapsed S]
  obol topup   --account A --amount N    add money to an account (admin)
  obol list                              list accounts and balances
  obol ping                              health-check the daemon

Global:
  --socket PATH   obold socket (default `+DefaultSocket+`)

settle KIND: complete | timeout | cancel | infrafail
`)
}

// socketFlag registers the shared --socket flag on a flag set.
func socketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", DefaultSocket, "obold Unix socket path")
}

// fail prints an error and returns exit code 1.
func fail(errOut io.Writer, err error) int {
	pf(errOut, "obol: %v\n", err)
	return 1
}

// pf and pl write to a CLI output stream. A write error to stdout/stderr is not
// actionable (the terminal is gone), so we deliberately discard it — centralizing
// that decision here rather than sprinkling `_, _ =` across every verb.
func pf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func pl(w io.Writer, a ...any)                { _, _ = fmt.Fprintln(w, a...) }

func cmdPing(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socket := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resp, err := roundTrip(*socket, wire.PingFrame())
	if err != nil {
		return fail(errOut, err)
	}
	if resp.MsgKind != wire.KindPing {
		return fail(errOut, fmt.Errorf("unexpected reply kind %q", resp.MsgKind))
	}
	pl(out, "ok")
	return 0
}
