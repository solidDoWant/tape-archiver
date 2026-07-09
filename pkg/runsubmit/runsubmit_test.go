package runsubmit

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const validConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

// opticalBurnConfigJSON is validConfigJSON with optical burning enabled, naming a
// real burner device. A dry-run must never leave this device in the submitted config.
const opticalBurnConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {
    "webhookUrl": "https://discord.com/api/webhooks/123/abc",
    "opticalBurn": {"drives": ["/dev/sr0"], "copies": 1, "allowNonBlankDiscs": true}
  }
}`

func parseConfig(t *testing.T, raw string) *config.Config {
	t.Helper()

	cfg, err := config.Parse([]byte(raw))
	require.NoError(t, err)

	return cfg
}

func TestApplyDryRun(t *testing.T) {
	t.Run("errors and rewrites nothing when env unset", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)

		err := ApplyDryRun(cfg, func(string) string { return "" }, &bytes.Buffer{})
		require.Error(t, err)

		message := err.Error()
		assert.Contains(t, message, MHVTLChangerEnv)
		assert.Contains(t, message, MHVTLDrive0Env)
		assert.Contains(t, message, MHVTLDrive1Env)

		// Fails closed: the original (real) devices are untouched.
		assert.Equal(t, "/dev/sch0", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst0", "/dev/nst1"}, cfg.Library.Drives)
	})

	t.Run("errors naming exactly the missing variable(s)", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)

		env := map[string]string{
			MHVTLChangerEnv: "/dev/sch9",
			MHVTLDrive0Env:  "/dev/nst8",
			// MHVTLDrive1Env deliberately unset.
		}

		err := ApplyDryRun(cfg, func(name string) string { return env[name] }, &bytes.Buffer{})
		require.Error(t, err)

		message := err.Error()
		assert.Contains(t, message, MHVTLDrive1Env)
		assert.NotContains(t, message, MHVTLChangerEnv)
		assert.NotContains(t, message, MHVTLDrive0Env)
	})

	t.Run("overrides devices to the mhvtl nodes", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)

		env := map[string]string{
			MHVTLChangerEnv: "/dev/sch9",
			MHVTLDrive0Env:  "/dev/nst8",
			MHVTLDrive1Env:  "/dev/nst9",
		}

		require.NoError(t, ApplyDryRun(cfg, func(name string) string { return env[name] }, &bytes.Buffer{}))

		assert.Equal(t, "/dev/sch9", cfg.Library.Changer)
		assert.Equal(t, []string{"/dev/nst8", "/dev/nst9"}, cfg.Library.Drives)
	})

	t.Run("disables optical burning and advises about it", func(t *testing.T) {
		cfg := parseConfig(t, opticalBurnConfigJSON)

		env := map[string]string{
			MHVTLChangerEnv: "/dev/sch9",
			MHVTLDrive0Env:  "/dev/nst8",
			MHVTLDrive1Env:  "/dev/nst9",
		}

		var warnings bytes.Buffer

		require.NoError(t, ApplyDryRun(cfg, func(name string) string { return env[name] }, &warnings))

		assert.Nil(t, cfg.Delivery.OpticalBurn)
		assert.Contains(t, warnings.String(), "optical burning disabled")
	})

	t.Run("no advisory when burning was not configured", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)

		env := map[string]string{
			MHVTLChangerEnv: "/dev/sch9",
			MHVTLDrive0Env:  "/dev/nst8",
			MHVTLDrive1Env:  "/dev/nst9",
		}

		var warnings bytes.Buffer

		require.NoError(t, ApplyDryRun(cfg, func(name string) string { return env[name] }, &warnings))
		assert.Empty(t, warnings.String())
	})

	t.Run("re-validates the config after the override", func(t *testing.T) {
		// Copies=2 but the dry-run always leaves exactly 2 mhvtl drives, so this
		// case exists mainly to prove Validate is actually invoked post-override;
		// a config with an invariant the override itself would break is exercised
		// implicitly by every other subtest succeeding.
		cfg := parseConfig(t, validConfigJSON)
		cfg.Copies = 0 // Trips Config.Validate's "copies: must be at least 1" rule.

		env := map[string]string{
			MHVTLChangerEnv: "/dev/sch9",
			MHVTLDrive0Env:  "/dev/nst8",
			MHVTLDrive1Env:  "/dev/nst9",
		}

		err := ApplyDryRun(cfg, func(name string) string { return env[name] }, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid config after dry-run override")
	})
}

func TestTranslateSubmitError(t *testing.T) {
	t.Run("already-started conflict is reported as actionable", func(t *testing.T) {
		started := serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-abc")

		err := TranslateSubmitError(started)
		require.Error(t, err)

		message := err.Error()
		assert.Contains(t, message, "already in progress")
		assert.Contains(t, message, backup.WorkflowID)
		assert.Contains(t, message, "run-abc")
		assert.Contains(t, message, "`tapectl status`")

		// The original serviceerror survives the translation via %w, so callers
		// can still classify it (e.g. pkg/runsapi mapping it to 409).
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		assert.True(t, errors.As(err, &alreadyStarted))
	})

	t.Run("other errors are wrapped verbatim", func(t *testing.T) {
		err := TranslateSubmitError(errors.New("boom"))
		require.Error(t, err)

		assert.Contains(t, err.Error(), "submit backup workflow")
		assert.Contains(t, err.Error(), "boom")
	})
}

// fakeSubmitClient is a hand-rolled fake of TemporalClient, exercising Submit's
// logic without a real Temporal connection.
type fakeSubmitClient struct {
	run      client.WorkflowRun
	err      error
	options  client.StartWorkflowOptions
	captured bool
}

func (f *fakeSubmitClient) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.options = options
	f.captured = true

	return f.run, f.err
}

func TestSubmit(t *testing.T) {
	t.Run("submits under the fixed singleton options", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)
		fake := &fakeSubmitClient{err: errors.New("connection refused")}

		_, err := Submit(context.Background(), fake, cfg)
		require.Error(t, err)

		require.True(t, fake.captured)
		assert.Equal(t, backup.WorkflowID, fake.options.ID)
		assert.Equal(t, backup.TaskQueue, fake.options.TaskQueue)
		assert.True(t, fake.options.WorkflowExecutionErrorWhenAlreadyStarted)
	})

	t.Run("a singleton conflict is translated, not returned raw", func(t *testing.T) {
		cfg := parseConfig(t, validConfigJSON)
		started := serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-abc")
		fake := &fakeSubmitClient{err: started}

		_, err := Submit(context.Background(), fake, cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already in progress")

		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		assert.True(t, errors.As(err, &alreadyStarted))
	})
}
