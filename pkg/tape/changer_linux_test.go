//go:build linux

package tape

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errCheckCondition stands in for the descriptive sense error sgCommand builds.
var errCheckCondition = errors.New("check condition")

func TestRetryUnitAttention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// outcomes is the (unitAttention, failed) result of each successive
		// attempt; the loop must not run more attempts than this supplies.
		outcomes     []struct{ unitAttention, failed bool }
		wantAttempts int
		wantErr      require.ErrorAssertionFunc
	}{
		{
			name: "a command that succeeds is issued once",
			outcomes: []struct{ unitAttention, failed bool }{
				{unitAttention: false, failed: false},
			},
			wantAttempts: 1,
		},
		{
			name: "a transient UNIT ATTENTION is absorbed and the retry succeeds",
			outcomes: []struct{ unitAttention, failed bool }{
				{unitAttention: true, failed: true},
				{unitAttention: false, failed: false},
			},
			wantAttempts: 2,
		},
		{
			name: "a run of UNIT ATTENTIONs is absorbed until one clears",
			outcomes: []struct{ unitAttention, failed bool }{
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
				{unitAttention: false, failed: false},
			},
			wantAttempts: 4,
		},
		{
			name: "any other check condition fails without a retry",
			outcomes: []struct{ unitAttention, failed bool }{
				{unitAttention: false, failed: true},
			},
			wantAttempts: 1,
			wantErr: func(t require.TestingT, err error, _ ...any) {
				require.ErrorIs(t, err, errCheckCondition)
			},
		},
		{
			name: "a persistent UNIT ATTENTION surfaces the sense once the attempts are spent",
			outcomes: []struct{ unitAttention, failed bool }{
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
				{unitAttention: true, failed: true},
			},
			wantAttempts: unitAttentionAttempts,
			wantErr: func(t require.TestingT, err error, _ ...any) {
				require.ErrorIs(t, err, errCheckCondition)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.wantErr == nil {
				test.wantErr = require.NoError
			}

			attempts := 0

			err := retryUnitAttention(t.Context(), 0, func() (bool, error) {
				require.Less(t, attempts, len(test.outcomes),
					"the retry issued more attempts than the case supplies outcomes for")

				outcome := test.outcomes[attempts]
				attempts++

				if !outcome.failed {
					return false, nil
				}

				return outcome.unitAttention, errCheckCondition
			})

			test.wantErr(t, err)
			assert.Equal(t, test.wantAttempts, attempts)
		})
	}
}

// A UNIT ATTENTION retry must stop when its context is cancelled.
func TestRetryUnitAttentionStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	attempts := 0

	// Long enough that cancellation, not the timer, ends the retry.
	err := retryUnitAttention(ctx, time.Minute, func() (bool, error) {
		attempts++

		cancel()

		return true, errCheckCondition
	})

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errCheckCondition, "the sense that prompted the retry must still be reported")
	assert.Equal(t, 1, attempts)
}
