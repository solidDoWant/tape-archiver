// Command tapectl is the operator CLI for tape-archiver. It submits a run
// config to Temporal as a backup workflow (optionally as a dry-run against the
// mhvtl virtual library) and inspects the status of running or completed runs.
//
// Temporal connection settings are read from the environment via the same
// envconfig loader the worker uses (TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, and
// the rest); see pkg/temporalclient.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

const usage = `tapectl — submit and inspect tape-archiver backup runs

Usage:
  tapectl run --config <file> [--dry-run] [--id <id>]
  tapectl status <workflow-id>
  tapectl resume <workflow-id>
  tapectl abort <workflow-id>

Commands:
  run     Submit a run config to Temporal as a backup workflow.
  status  Print a workflow's status and last completed phase.
  resume  Resume a paused run: after you clear the I/O station (Eject pause), or
          after you swap failed tapes for fresh blanks (Load/Write pause).
  abort   Abort a run paused on a Load/Write failure, ending it with no more writes.

Connection is configured via TEMPORAL_ADDRESS (and TEMPORAL_NAMESPACE, etc.).
`

// getenv reads environment variables. It is a package variable so tests can
// substitute a lookup without mutating the process environment.
var getenv = os.Getenv

func main() {
	if err := dispatch(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "tapectl: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes to the requested subcommand. It is separated from main so
// tests can drive the CLI with explicit args, output, and context.
func dispatch(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("a command is required\n\n%s", usage)
	}

	command, rest := args[0], args[1:]
	switch command {
	case "run":
		return submitRun(ctx, rest, out)
	case "status":
		return showStatus(ctx, rest, out)
	case "resume":
		return resumeRun(ctx, rest, out)
	case "abort":
		return abortRun(ctx, rest, out)
	case "-h", "--help", "help":
		_, err := fmt.Fprint(out, usage)

		return err
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

// requireTemporalAddress fails fast with a descriptive error when the Temporal
// frontend address is unset, before any connection is attempted. getenv is
// injected so tests can exercise both branches without mutating the process
// environment.
func requireTemporalAddress(getenv func(string) string) error {
	if getenv("TEMPORAL_ADDRESS") == "" {
		return fmt.Errorf("TEMPORAL_ADDRESS is not set; set it to the Temporal frontend address (e.g. localhost:7233)")
	}

	return nil
}
