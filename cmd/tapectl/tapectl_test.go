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
  "redundancy": {"targetPercentage": 10},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

// opticalBurnConfigJSON names a real burner, which a dry-run must drop.
const opticalBurnConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {
    "webhookUrl": "https://discord.com/api/webhooks/123/abc",
    "opticalBurn": {"drives": ["/dev/sr0"], "copies": 1, "allowNonBlankDiscs": true}
  }
}`

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
	// Must fail closed rather than fall back to the real library's devices.
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

	// mhvtl has no virtual burner, so a dry-run must not drive the real one.
	t.Run("disables optical burning and drops the real burner device", func(t *testing.T) {
		env := map[string]string{
			mhvtlChangerEnv: "/dev/sch9",
			mhvtlDrive0Env:  "/dev/nst8",
			mhvtlDrive1Env:  "/dev/nst9",
		}

		withGetenv(t, func(name string) string { return env[name] })
		warnings := withWarnOut(t)

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, opticalBurnConfigJSON), dryRun: true})
		require.NoError(t, err)
		require.NotNil(t, cfg)

		assert.False(t, cfg.Delivery.OpticalBurn.Enabled())
		assert.Nil(t, cfg.Delivery.OpticalBurn)

		assert.Equal(t, "/dev/sch9", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst8", "/dev/nst9"}, cfg.Library.Drives)

		assert.Contains(t, warnings.String(), "optical burning disabled")
	})

	t.Run("no optical-burn advisory when burning is not configured", func(t *testing.T) {
		env := map[string]string{
			mhvtlChangerEnv: "/dev/sch9",
			mhvtlDrive0Env:  "/dev/nst8",
			mhvtlDrive1Env:  "/dev/nst9",
		}

		withGetenv(t, func(name string) string { return env[name] })
		warnings := withWarnOut(t)

		cfg, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true})
		require.NoError(t, err)
		assert.Nil(t, cfg.Delivery.OpticalBurn)
		assert.Empty(t, warnings.String())
	})
}

func TestParseRunArgsRejectsID(t *testing.T) {
	_, err := parseRunArgs([]string{"--config", "run.json", "--id", "my-run"})
	require.Error(t, err)
}

// TestParseRunArgsRejectsStrayPositional guards against a trailing --dry-run
// being silently dropped after a stray positional.
func TestParseRunArgsRejectsStrayPositional(t *testing.T) {
	options, err := parseRunArgs([]string{"--config", "run.json", "backup", "--dry-run"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup")
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

		// The suggested command must work when copy-pasted verbatim.
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

func TestSubmitRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := submitRun(context.Background(), []string{"--config", writeConfig(t, validConfigJSON)}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

func TestRequireNoArgs(t *testing.T) {
	require.NoError(t, requireNoArgs("status", nil))
	require.NoError(t, requireNoArgs("resume", nil))
	require.NoError(t, requireNoArgs("abort", nil))
	require.NoError(t, requireNoArgs("cancel", nil))

	err := requireNoArgs("resume", []string{"backup"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no arguments")

	err = requireNoArgs("abort", []string{"a", "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no arguments")
}

func TestResumeRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := resumeRun(context.Background(), nil, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

func TestAbortRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := abortRun(context.Background(), nil, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_ADDRESS")
}

func TestCancelRunMissingTemporalAddress(t *testing.T) {
	withGetenv(t, func(string) string { return "" })

	err := cancelRun(context.Background(), nil, &bytes.Buffer{})
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

// suggestedStatusArgs returns the arguments of the backticked `tapectl status`
// command in message, or nil if there is none.
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

func withGetenv(t *testing.T, fn func(string) string) {
	t.Helper()

	original := getenv
	getenv = fn

	t.Cleanup(func() { getenv = original })
}

func withWarnOut(t *testing.T) *bytes.Buffer {
	t.Helper()

	original := warnOut
	buffer := &bytes.Buffer{}
	warnOut = buffer

	t.Cleanup(func() { warnOut = original })

	return buffer
}
