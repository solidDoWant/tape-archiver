package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

// writeConfig writes content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestBuildSubmission(t *testing.T) {
	t.Run("loads config and keeps configured devices", func(t *testing.T) {
		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON)})
		require.NoError(t, err)

		assert.Equal(t, "/dev/sch0", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst0", "/dev/nst1"}, cfg.Library.Drives)
	})

	t.Run("missing --config is an error", func(t *testing.T) {
		_, err := buildSubmission(runOptions{})
		require.Error(t, err)
	})

	t.Run("invalid config is rejected before submission", func(t *testing.T) {
		_, err := buildSubmission(runOptions{configPath: writeConfig(t, `{"copies": 2}`)})
		require.Error(t, err)
	})
}

func TestBuildSubmissionDryRun(t *testing.T) {
	// AC1: a dry-run with no MHVTL_* set must fail closed rather than fall back
	// to device nodes that are byte-identical to the real library.
	t.Run("errors and submits nothing when env unset", func(t *testing.T) {
		withGetenv(t, func(string) string { return "" })

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true})
		require.Error(t, err)
		assert.Nil(t, cfg)

		message := err.Error()
		assert.Contains(t, message, mhvtlChangerEnv)
		assert.Contains(t, message, mhvtlDrive0Env)
		assert.Contains(t, message, mhvtlDrive1Env)
	})

	// AC1: the error names exactly which variable(s) are missing so the operator
	// can fix it, and it must not silently proceed on a partial override.
	t.Run("errors naming the single missing variable", func(t *testing.T) {
		env := map[string]string{
			mhvtlChangerEnv: "/dev/sch9",
			mhvtlDrive0Env:  "/dev/nst8",
			// mhvtlDrive1Env deliberately unset.
		}

		withGetenv(t, func(name string) string { return env[name] })

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true})
		require.Error(t, err)
		assert.Nil(t, cfg)

		message := err.Error()
		assert.Contains(t, message, mhvtlDrive1Env)
		assert.NotContains(t, message, mhvtlChangerEnv)
		assert.NotContains(t, message, mhvtlDrive0Env)
	})

	t.Run("env overrides mhvtl device paths", func(t *testing.T) {
		env := map[string]string{
			mhvtlChangerEnv: "/dev/sch9",
			mhvtlDrive0Env:  "/dev/nst8",
			mhvtlDrive1Env:  "/dev/nst9",
		}

		withGetenv(t, func(name string) string { return env[name] })

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true})
		require.NoError(t, err)

		assert.Equal(t, "/dev/sch9", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst8", "/dev/nst9"}, cfg.Library.Drives)
	})
}

// TestParseRunArgsRejectsID guards the removal of the --id flag: with the
// singleton workflow ID there is no way to submit under a distinct ID.
func TestParseRunArgsRejectsID(t *testing.T) {
	_, err := parseRunArgs([]string{"--config", "run.json", "--id", "my-run"})
	require.Error(t, err)
}

// TestParseRunArgsRejectsStrayPositional covers AC2: a stray positional
// argument must be rejected naming the unexpected argument. Go's flag package
// stops at the first positional, so without this guard a trailing --dry-run is
// silently dropped and a real run is submitted where a test was intended.
func TestParseRunArgsRejectsStrayPositional(t *testing.T) {
	options, err := parseRunArgs([]string{"--config", "run.json", "backup", "--dry-run"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup")
	// The dropped --dry-run must not have been silently honored.
	assert.False(t, options.dryRun)
}

func TestTranslateSubmitError(t *testing.T) {
	t.Run("already-started conflict is reported as actionable", func(t *testing.T) {
		started := serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-abc")

		err := translateSubmitError(started)
		require.Error(t, err)

		message := err.Error()
		assert.Contains(t, message, "already in progress")
		assert.Contains(t, message, backupWorkflowID)
		assert.Contains(t, message, "run-abc")

		// AC3: the suggested inspection command must execute successfully when
		// copy-pasted verbatim. `tapectl status` takes no arguments
		// (requireNoArgs), so the suggestion must be bare `tapectl status` and
		// must not append the workflow ID as an argument.
		assert.Contains(t, message, "`tapectl status`")
		assert.NotContains(t, message, "tapectl status "+backupWorkflowID)
		require.NoError(t, requireNoArgs("status", suggestedStatusArgs(message)))
	})

	t.Run("other errors are wrapped verbatim", func(t *testing.T) {
		err := translateSubmitError(errors.New("boom"))
		require.Error(t, err)

		assert.Contains(t, err.Error(), "submit backup workflow")
		assert.Contains(t, err.Error(), "boom")
	})
}

func TestRequireTemporalAddress(t *testing.T) {
	assert.Error(t, requireTemporalAddress(func(string) string { return "" }))
	assert.NoError(t, requireTemporalAddress(func(name string) string {
		if name == "TEMPORAL_ADDRESS" {
			return "localhost:7233"
		}

		return ""
	}))
}

// TestSubmitRunMissingTemporalAddress exercises the full run path with a valid
// config but no TEMPORAL_ADDRESS: it must fail with a descriptive error and
// never attempt a connection.
func TestSubmitRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := submitRun(context.Background(), []string{"--config", writeConfig(t, validConfigJSON)}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

// TestRequireNoArgs covers the singleton subcommands' argument handling: they take
// no arguments (every run is the singleton backupWorkflowID), so no args is valid
// and any positional argument is rejected.
func TestRequireNoArgs(t *testing.T) {
	require.NoError(t, requireNoArgs("status", nil))
	require.NoError(t, requireNoArgs("resume", nil))
	require.NoError(t, requireNoArgs("abort", nil))

	err := requireNoArgs("resume", []string{"backup"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no arguments")

	err = requireNoArgs("abort", []string{"a", "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no arguments")
}

// TestResumeRunMissingTemporalAddress exercises the resume path with no
// TEMPORAL_ADDRESS: it must fail with a descriptive error and never attempt a
// connection.
func TestResumeRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := resumeRun(context.Background(), nil, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

// TestAbortRunMissingTemporalAddress exercises the abort path with no
// TEMPORAL_ADDRESS: it must fail with a descriptive error and never attempt a
// connection.
func TestAbortRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := abortRun(context.Background(), nil, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

func TestFormatStatus(t *testing.T) {
	tests := []struct {
		name   string
		status enumspb.WorkflowExecutionStatus
		want   string
	}{
		{name: "running", status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, want: "Running"},
		{name: "completed", status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, want: "Completed"},
		{name: "multi-word", status: enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW, want: "ContinuedAsNew"},
		{name: "unspecified", status: enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, want: "Unspecified"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, formatStatus(test.status))
		})
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	err := dispatch(context.Background(), []string{"bogus"}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestDispatchHelp(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, dispatch(context.Background(), []string{"--help"}, &out))
	assert.True(t, strings.Contains(out.String(), "tapectl"))
}

// suggestedStatusArgs extracts the arguments of the backticked `tapectl status`
// command embedded in an error message and returns the tokens after
// `tapectl status`. It lets a test feed the tool's own suggestion straight into
// requireNoArgs, proving the copy-pasted command parses (AC3). If the message
// contains no backticked `tapectl status` command it returns nil.
func suggestedStatusArgs(message string) []string {
	const prefix = "`tapectl status"

	start := strings.Index(message, prefix)
	if start < 0 {
		return nil
	}

	rest := message[start+len(prefix):]

	end := strings.Index(rest, "`")
	if end < 0 {
		return nil
	}

	return strings.Fields(rest[:end])
}

// withGetenv swaps the package getenv for the duration of a test.
func withGetenv(t *testing.T, fn func(string) string) {
	t.Helper()

	original := getenv
	getenv = fn

	t.Cleanup(func() { getenv = original })
}
