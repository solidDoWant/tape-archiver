package backup

import (
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// TestDecideBurn exercises the overwrite policy for every disc state × opt-in
// combination. This is the "never silently overwrite" decision ACs 3–5 turn on,
// unit-tested without any optical hardware (the physical round-trips live in
// burn_integration_test.go).
func TestDecideBurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		state                optical.DiscState
		allowNonBlank        bool
		driveHasVerifiedCopy bool
		want                 burnAction
		requireError         require.ErrorAssertionFunc
	}{
		{
			name:  "blank disc writes directly (default)",
			state: optical.StateBlank,
			want:  burnWrite,
		},
		{
			name:          "blank disc writes directly (opt-in irrelevant)",
			state:         optical.StateBlank,
			allowNonBlank: true,
			want:          burnWrite,
		},
		{
			// A freshly-swapped blank in a drive that already burned a copy still
			// writes: the issue #154 guard only ever refuses a NON-blank disc.
			name:                 "blank disc in a reused drive writes directly (issue #154)",
			state:                optical.StateBlank,
			allowNonBlank:        true,
			driveHasVerifiedCopy: true,
			want:                 burnWrite,
		},
		{
			name:  "non-blank rewritable without opt-in pauses (AC3)",
			state: optical.StateNonBlankRewritable,
			want:  burnPause,
		},
		{
			name:          "non-blank rewritable with opt-in reclaims and writes (AC4)",
			state:         optical.StateNonBlankRewritable,
			allowNonBlank: true,
			want:          burnReclaimWrite,
		},
		{
			// The core issue #154 fix: even with the opt-in, a non-blank rewritable
			// disc in a drive that already verified a copy this run is that copy still
			// loaded — never reclaim it, pause for a fresh blank instead.
			name:                 "non-blank rewritable with opt-in but drive already verified a copy pauses (issue #154)",
			state:                optical.StateNonBlankRewritable,
			allowNonBlank:        true,
			driveHasVerifiedCopy: true,
			want:                 burnPause,
		},
		{
			name:  "appendable write-once without opt-in pauses",
			state: optical.StateAppendableWriteOnce,
			want:  burnPause,
		},
		{
			name:          "appendable write-once with opt-in still pauses (AC5)",
			state:         optical.StateAppendableWriteOnce,
			allowNonBlank: true,
			want:          burnPause,
		},
		{
			name:  "finalized write-once without opt-in pauses",
			state: optical.StateFinalized,
			want:  burnPause,
		},
		{
			name:          "finalized write-once with opt-in still pauses (AC5)",
			state:         optical.StateFinalized,
			allowNonBlank: true,
			want:          burnPause,
		},
		{
			name:         "unknown state is a hard error",
			state:        optical.StateUnknown,
			requireError: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			requireError := test.requireError
			if requireError == nil {
				requireError = require.NoError
			}

			action, err := decideBurn(test.state, test.allowNonBlank, test.driveHasVerifiedCopy)
			requireError(t, err)

			if test.requireError == nil {
				assert.Equal(t, test.want, action)
			}
		})
	}
}

// TestDiscNotWritableError asserts the operator-pause error BurnDisc builds is
// distinguishable via IsDiscNotWritable, names the drive, and is non-retryable,
// while unrelated errors are not misclassified.
func TestDiscNotWritableError(t *testing.T) {
	t.Parallel()

	t.Run("rewritable without opt-in", func(t *testing.T) {
		t.Parallel()

		err := discNotWritableError("/dev/sr0", optical.StateNonBlankRewritable, false, false)

		assert.True(t, IsDiscNotWritable(err), "should be recognized as the disc-not-writable pause error")
		assert.ErrorContains(t, err, "/dev/sr0")
		assert.ErrorContains(t, err, "AllowNonBlankDiscs")

		var appErr *temporal.ApplicationError
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, DiscNotWritableErrorType, appErr.Type())
		assert.True(t, appErr.NonRetryable(), "the pause error must be non-retryable")
	})

	t.Run("write-once even with opt-in", func(t *testing.T) {
		t.Parallel()

		err := discNotWritableError("/dev/sr1", optical.StateFinalized, true, false)

		assert.True(t, IsDiscNotWritable(err))
		assert.ErrorContains(t, err, "/dev/sr1")
		assert.ErrorContains(t, err, "write-once")
	})

	t.Run("this run's own verified copy still loaded (issue #154)", func(t *testing.T) {
		t.Parallel()

		// A non-blank rewritable disc in a drive that already verified a copy, with
		// the opt-in set: the reason must name it as this run's own copy, not a
		// generic AllowNonBlankDiscs refusal, so the operator swaps rather than
		// enabling the opt-in.
		err := discNotWritableError("/dev/sr0", optical.StateNonBlankRewritable, true, true)

		assert.True(t, IsDiscNotWritable(err), "should be recognized as the disc-not-writable pause error")
		assert.ErrorContains(t, err, "/dev/sr0")
		assert.ErrorContains(t, err, "already burned and verified")
		assert.ErrorContains(t, err, "fresh blank")
	})

	t.Run("unrelated errors are not disc-not-writable", func(t *testing.T) {
		t.Parallel()

		assert.False(t, IsDiscNotWritable(nil))
		assert.False(t, IsDiscNotWritable(errors.New("some other failure")))
		assert.False(t, IsDiscNotWritable(fmt.Errorf("wrapped: %w",
			temporal.NewNonRetryableApplicationError("x", "other-type", nil))))
	})
}
