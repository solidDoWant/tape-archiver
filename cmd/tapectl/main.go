// Command tapectl is the operator CLI for tape-archiver: it submits backup runs
// to Temporal and inspects or controls the running one. Temporal settings come
// from the environment (see pkg/temporalclient).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

const usage = `tapectl — submit and inspect tape-archiver backup runs

Usage:
  tapectl run --config <file> [--dry-run]
  tapectl status
  tapectl resume
  tapectl abort
  tapectl cancel

Commands:
  run     Submit a run config to Temporal as a backup workflow.
  status  Print the backup run's status and last completed phase.
  resume  Resume the paused backup run: after you clear the I/O station (Eject
          pause), or after you swap failed tapes for fresh blanks (Load/Write pause).
  abort   Abort the backup run paused on a Load/Write failure, ending it with no
          more writes.
  cancel  Cancel the in-progress backup run (paused or not), stopping it
          gracefully: it tears down its tape mounts and closes as Canceled.

Runs are a singleton (SPEC §4.2), so status, resume, abort, and cancel take no
arguments; they act on the one backup run. Connection is configured via
TEMPORAL_ADDRESS (and TEMPORAL_NAMESPACE, etc.).
`

// getenv is a var so tests can substitute it.
var getenv = os.Getenv

func main() {
	if err := dispatch(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "tapectl: %v\n", err)
		os.Exit(1)
	}
}

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
	case "cancel":
		return cancelRun(ctx, rest, out)
	case "-h", "--help", "help":
		_, err := fmt.Fprint(out, usage)

		return err
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

func requireTemporalAddress(getenv func(string) string) error {
	if getenv("TEMPORAL_ADDRESS") == "" {
		return fmt.Errorf("TEMPORAL_ADDRESS is not set; set it to the Temporal frontend address (e.g. localhost:7233)")
	}

	return nil
}

// requireNoArgs rejects positional arguments for the subcommands that act on
// the singleton run.
func requireNoArgs(command string, args []string) error {
	flagSet := flag.NewFlagSet(command, flag.ContinueOnError)
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if flagSet.NArg() != 0 {
		return fmt.Errorf("tapectl %s takes no arguments\n\nUsage: tapectl %s", command, command)
	}

	return nil
}
