// Command obold is the budget sidecar daemon. It holds the budget kernel and
// serves the gate to Slurm's job_submit shim over a local socket; the obol CLI
// (management/admin) talks to it over the same socket.
//
// See docs/SEAM_DESIGN.md. This is a stub; the protocol server is tracked in
// the GitHub milestone "v0.1.0 — obold MVP".
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("obold %s\n", version)
		return
	}
	fmt.Fprintln(os.Stderr, "obold: not yet implemented — see docs/SEAM_DESIGN.md")
	os.Exit(1)
}
