package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

const (
	testSource    = "tape_test/archive@test-snap"
	testRecipient = "age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"
	testIdentity  = "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"
)

func TestBuildSeedConfig(t *testing.T) {
	cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 21, "")

	require.Len(t, cfg.Sources, 1)
	require.NotNil(t, cfg.Sources[0].ZFSPath)
	assert.Equal(t, testSource, cfg.Sources[0].ZFSPath.Name)
	assert.Equal(t, 1, cfg.Copies)
	assert.Equal(t, []int{21}, cfg.Library.BlankSlots)
	assert.True(t, cfg.Library.AllowNonBlankTapes, "seed configs must tolerate reusing an already-written dev slot")
	assert.Equal(t, []string{testRecipient}, cfg.Encryption.Recipients)
	assert.Equal(t, testIdentity, cfg.Encryption.Identity)
	assert.Empty(t, cfg.Delivery.WebhookURL, "an unset webhook must leave delivery a no-op (empty WebhookURL)")

	// The config must be structurally valid on its own (before runsubmit.ApplyDryRun
	// rewrites the placeholder changer/drives) — an invalid seed config would be a
	// bug in buildSeedConfig, not something ApplyDryRun/Submit should ever surface.
	assert.NoError(t, cfg.Validate())
}

func TestBuildSeedConfig_threadsWebhookURL(t *testing.T) {
	// `make web-dev` passes the local fake Discord receiver's URL so seeded runs
	// deliver a report and the "Discord report ↗" deep-link renders locally.
	const webhookURL = "http://127.0.0.1:9997/webhook/dev"

	cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 21, webhookURL)

	assert.Equal(t, webhookURL, cfg.Delivery.WebhookURL)
	assert.NoError(t, cfg.Validate())
}

func TestBuildSeedConfig_slotVariesByCall(t *testing.T) {
	first := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")
	second := buildSeedConfig(testSource, testRecipient, testIdentity, 21, "")

	assert.NotEqual(t, first.Library.BlankSlots, second.Library.BlankSlots)
}

func TestEffectiveFailCount(t *testing.T) {
	tests := []struct {
		name           string
		failCount      int
		failWebhookURL string
		want           int
	}{
		{
			name:           "keeps the count when a reject webhook is configured",
			failCount:      2,
			failWebhookURL: "http://127.0.0.1:9997/webhook/reject",
			want:           2,
		},
		{
			name:           "drops to zero with no reject webhook (nothing to fail against)",
			failCount:      2,
			failWebhookURL: "",
			want:           0,
		},
		{
			name:           "zero stays zero regardless of webhook",
			failCount:      0,
			failWebhookURL: "",
			want:           0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, effectiveFailCount(test.failCount, test.failWebhookURL))
		})
	}
}

func TestEnvNonNegativeInt(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fallback int
		want     int
		wantErr  require.ErrorAssertionFunc
	}{
		{name: "unset uses the fallback", raw: "", fallback: 1, want: 1, wantErr: require.NoError},
		{name: "zero is accepted", raw: "0", fallback: 1, want: 0, wantErr: require.NoError},
		{name: "positive is accepted", raw: "2", fallback: 1, want: 2, wantErr: require.NoError},
		{name: "negative is rejected", raw: "-1", fallback: 1, wantErr: require.Error},
		{name: "non-numeric is rejected", raw: "lots", fallback: 1, wantErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(string) string { return test.raw }

			got, err := envNonNegativeInt(getenv, "WEBDEVSEED_FAIL_COUNT", test.fallback)
			test.wantErr(t, err)

			if err == nil {
				assert.Equal(t, test.want, got)
			}
		})
	}
}

// fakeSubmitClient is a hand-rolled fake of runsubmit.TemporalClient (mirroring
// pkg/runsubmit's own test double of the same interface), driving
// submitWithRetry's logic without a real Temporal connection: it returns a
// WorkflowExecutionAlreadyStarted conflict for the first conflictsBeforeSuccess
// calls, then succeeds.
type fakeSubmitClient struct {
	conflictsBeforeSuccess int
	calls                  int
	run                    client.WorkflowRun
}

func (f *fakeSubmitClient) ExecuteWorkflow(_ context.Context, _ client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.calls++

	if f.calls <= f.conflictsBeforeSuccess {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-abc")
	}

	return f.run, nil
}

func TestSubmitWithRetry(t *testing.T) {
	t.Run("succeeds immediately with no conflict", func(t *testing.T) {
		fake := &fakeSubmitClient{conflictsBeforeSuccess: 0}
		cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")

		_, err := submitWithRetry(t.Context(), fake, cfg)
		require.NoError(t, err)
		assert.Equal(t, 1, fake.calls)
	})

	t.Run("retries through a singleton conflict and eventually succeeds", func(t *testing.T) {
		restoreRetryWait(t)

		submitRetryWait = time.Millisecond

		fake := &fakeSubmitClient{conflictsBeforeSuccess: 3}
		cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")

		_, err := submitWithRetry(t.Context(), fake, cfg)
		require.NoError(t, err)
		assert.Equal(t, 4, fake.calls)
	})

	t.Run("gives up after maxSubmitAttempts and returns the conflict", func(t *testing.T) {
		restoreRetryWait(t)

		submitRetryWait = time.Millisecond

		fake := &fakeSubmitClient{conflictsBeforeSuccess: maxSubmitAttempts + 1}
		cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")

		_, err := submitWithRetry(t.Context(), fake, cfg)
		require.Error(t, err)
		assert.Equal(t, maxSubmitAttempts, fake.calls)

		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		assert.True(t, errors.As(err, &alreadyStarted))
	})

	t.Run("a non-conflict error is not retried", func(t *testing.T) {
		fake := &conflictOnceThenOtherErrorClient{}
		cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")

		_, err := submitWithRetry(t.Context(), fake, cfg)
		require.Error(t, err)
		assert.Equal(t, 1, fake.calls)
		assert.NotContains(t, err.Error(), "retries")
	})

	t.Run("context cancellation stops the retry wait", func(t *testing.T) {
		restoreRetryWait(t)

		submitRetryWait = time.Hour

		fake := &fakeSubmitClient{conflictsBeforeSuccess: maxSubmitAttempts + 1}
		cfg := buildSeedConfig(testSource, testRecipient, testIdentity, 20, "")

		ctx, cancel := context.WithCancel(t.Context())

		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		_, err := submitWithRetry(ctx, fake, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

// conflictOnceThenOtherErrorClient always fails with a plain (non-conflict)
// error, verifying submitWithRetry does not retry an error that is not a
// singleton conflict.
type conflictOnceThenOtherErrorClient struct {
	calls int
}

func (f *conflictOnceThenOtherErrorClient) ExecuteWorkflow(_ context.Context, _ client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.calls++

	return nil, errors.New("connection refused")
}

// restoreRetryWait resets the package-level submitRetryWait var to its
// production default after a test overrides it.
func restoreRetryWait(t *testing.T) {
	t.Helper()

	original := submitRetryWait

	t.Cleanup(func() { submitRetryWait = original })
}

func TestEnvOr(t *testing.T) {
	env := map[string]string{"SET": "value"}
	getenv := func(name string) string { return env[name] }

	assert.Equal(t, "value", envOr(getenv, "SET", "fallback"))
	assert.Equal(t, "fallback", envOr(getenv, "UNSET", "fallback"))
}

func TestEnvInt(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantErr   require.ErrorAssertionFunc
		wantValue int
	}{
		{
			name:      "unset uses fallback",
			env:       map[string]string{},
			wantErr:   require.NoError,
			wantValue: 3,
		},
		{
			name:      "valid positive integer",
			env:       map[string]string{"COUNT": "5"},
			wantErr:   require.NoError,
			wantValue: 5,
		},
		{
			name:    "not an integer",
			env:     map[string]string{"COUNT": "banana"},
			wantErr: require.Error,
		},
		{
			name:    "zero is rejected",
			env:     map[string]string{"COUNT": "0"},
			wantErr: require.Error,
		},
		{
			name:    "negative is rejected",
			env:     map[string]string{"COUNT": "-1"},
			wantErr: require.Error,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			getenv := func(name string) string { return testCase.env[name] }

			value, err := envInt(getenv, "COUNT", 3)
			testCase.wantErr(t, err)

			if err == nil {
				assert.Equal(t, testCase.wantValue, value)
			}
		})
	}
}

// Compile-time sanity check that config.Config from buildSeedConfig is the
// type submitWithRetry/runsubmit.Submit actually expect.
var _ = func() *config.Config { return buildSeedConfig("", "", "", 0, "") }
