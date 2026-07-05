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
	t.Run("default mhvtl devices when env unset", func(t *testing.T) {
		withGetenv(t, func(string) string { return "" })

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true})
		require.NoError(t, err)

		assert.Equal(t, defaultMHVTLChanger, cfg.Library.Changer)
		assert.Equal(t, []string{defaultMHVTLDrive0, defaultMHVTLDrive1}, cfg.Library.Drives)
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

func TestTranslateSubmitError(t *testing.T) {
	t.Run("already-started conflict is reported as actionable", func(t *testing.T) {
		started := serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-abc")

		err := translateSubmitError(started)
		require.Error(t, err)

		message := err.Error()
		assert.Contains(t, message, "already in progress")
		assert.Contains(t, message, backupWorkflowID)
		assert.Contains(t, message, "run-abc")
		assert.Contains(t, message, "tapectl status")
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

// withGetenv swaps the package getenv for the duration of a test.
func withGetenv(t *testing.T, fn func(string) string) {
	t.Helper()

	original := getenv
	getenv = fn

	t.Cleanup(func() { getenv = original })
}
