// Command obol is the budget-management CLI. It talks to the obold daemon over
// its Unix socket (decision #19: the daemon is the single authority over budget
// state). See internal/cli for the verbs.
package main

import (
	"fmt"
	"os"

	"github.com/scttfrdmn/obol/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("obol %s\n", version)
		return
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
