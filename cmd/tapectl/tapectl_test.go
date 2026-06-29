package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"]},
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
	now := time.Date(2026, 6, 29, 13, 45, 5, 0, time.UTC)

	t.Run("generates timestamped ID and keeps configured devices", func(t *testing.T) {
		cfg, id, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON)}, now)
		require.NoError(t, err)

		assert.Equal(t, "backup-20260629T134505Z", id)
		assert.Equal(t, "/dev/sch0", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst0", "/dev/nst1"}, cfg.Library.Drives)
	})

	t.Run("operator-supplied ID overrides the default", func(t *testing.T) {
		_, id, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), id: "my-run"}, now)
		require.NoError(t, err)

		assert.Equal(t, "my-run", id)
	})

	t.Run("missing --config is an error", func(t *testing.T) {
		_, _, err := buildSubmission(runOptions{}, now)
		require.Error(t, err)
	})

	t.Run("invalid config is rejected before submission", func(t *testing.T) {
		_, _, err := buildSubmission(runOptions{configPath: writeConfig(t, `{"copies": 2}`)}, now)
		require.Error(t, err)
	})
}

func TestBuildSubmissionDryRun(t *testing.T) {
	now := time.Date(2026, 6, 29, 13, 45, 5, 0, time.UTC)

	t.Run("default mhvtl devices when env unset", func(t *testing.T) {
		withGetenv(t, func(string) string { return "" })

		cfg, _, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true}, now)
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

		cfg, _, err := buildSubmission(runOptions{configPath: writeConfig(t, validConfigJSON), dryRun: true}, now)
		require.NoError(t, err)

		assert.Equal(t, "/dev/sch9", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst8", "/dev/nst9"}, cfg.Library.Drives)
	})
}

func TestGenerateWorkflowID(t *testing.T) {
	// A non-UTC input must still render as UTC.
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.FixedZone("PST", -8*3600))
	assert.Equal(t, "backup-20260102T230405Z", generateWorkflowID(now))
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

func TestParseStatusArgs(t *testing.T) {
	id, err := parseStatusArgs([]string{"backup-123"})
	require.NoError(t, err)
	assert.Equal(t, "backup-123", id)

	_, err = parseStatusArgs(nil)
	require.Error(t, err)

	_, err = parseStatusArgs([]string{"a", "b"})
	require.Error(t, err)
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
