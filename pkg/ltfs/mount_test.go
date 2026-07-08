package ltfs

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnescapeMountinfo covers the octal unescaping applied to the mount-point
// field of /proc/self/mountinfo (space, tab, newline, backslash), which
// mountpointInUse relies on to match paths verbatim.
func TestUnescapeMountinfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		want  string
	}{
		{name: "no escapes", field: "/mnt/tape", want: "/mnt/tape"},
		{name: "space", field: `/mnt/my\040tape`, want: "/mnt/my tape"},
		{name: "tab", field: `/mnt/a\011b`, want: "/mnt/a\tb"},
		{name: "newline", field: `/mnt/a\012b`, want: "/mnt/a\nb"},
		{name: "backslash", field: `/mnt/a\134b`, want: `/mnt/a\b`},
		{name: "multiple", field: `/a\040b\040c`, want: "/a b c"},
		{name: "trailing lone backslash left intact", field: `/mnt/tape\`, want: `/mnt/tape\`},
		{name: "non-octal sequence left intact", field: `/mnt/ta\pe`, want: `/mnt/ta\pe`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, unescapeMountinfo(test.field))
		})
	}
}

// TestMountpointInUse verifies the /proc/self/mountinfo scan reports well-known
// mount points as in use and ordinary paths as not, without stat'ing the path
// (the property that lets it detect an orphaned ENOTCONN mount). This is the
// attribution guard's core logic, exercised without a tape drive.
func TestMountpointInUse(t *testing.T) {
	t.Parallel()

	// "/" is always a mount point and always present in mountinfo.
	inUse, err := mountpointInUse("/")
	require.NoError(t, err)
	assert.True(t, inUse, "/ must be reported as a mount point")

	// A path that exists but is an ordinary directory, not a mount boundary.
	inUse, err = mountpointInUse(t.TempDir())
	require.NoError(t, err)
	assert.False(t, inUse, "a fresh temp dir is not a mount point")

	// A path whose final component does not exist must not error (no stat of the
	// component): the parent is resolved, the missing path simply is not found in
	// mountinfo. This mirrors the pre-MkdirAll call in Volume.Mount.
	inUse, err = mountpointInUse(t.TempDir() + "/does-not-exist")
	require.NoError(t, err)
	assert.False(t, inUse, "a non-existent path is not a mount point")
}

// closedDone returns an already-closed channel, standing in for a supervised
// ltfs process that has already exited.
func closedDone() chan struct{} {
	done := make(chan struct{})
	close(done)

	return done
}

// TestUnmountIdempotentAfterDetach proves AC1's retry path deterministically,
// without a tape drive: once a mount is detached, a second Unmount must NOT
// re-run fusermount -u (which would exit non-zero on the already-released
// mountpoint and spuriously fail the retry). The mountpoint here is not mounted,
// so any fusermount call would fail — a nil return proves it was skipped.
func TestUnmountIdempotentAfterDetach(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("index write blew up")

	tests := []struct {
		name    string
		waitErr error
		wantErr require.ErrorAssertionFunc
	}{
		{name: "clean index write", waitErr: nil, wantErr: require.NoError},
		{name: "failed index write still surfaces", waitErr: writeErr, wantErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			m := &Mount{
				mountpoint: t.TempDir(), // exists but is not a mount point
				detached:   true,        // a prior Unmount already released it
				done:       closedDone(),
				waitErr:    test.waitErr,
			}

			err := m.Unmount(t.Context())
			test.wantErr(t, err)
		})
	}
}

// TestUnmountReturnsWhenContextCancelled proves the seam issue #223's teardown fix
// relies on: Unmount's terminal wait for the LTFS index write honours the context,
// so bounding that context (TeardownSession wraps its uncancellable WithoutCancel
// context in a WithTimeout) makes Unmount return — letting MountRegistry.Teardown
// fall through to Kill — instead of blocking forever. Here the supervised process
// never exits (done is never closed) and the context is already cancelled, so
// Unmount must return the context error promptly rather than hang.
func TestUnmountReturnsWhenContextCancelled(t *testing.T) {
	t.Parallel()

	m := &Mount{
		mountpoint: t.TempDir(),         // exists but is not a mount point
		detached:   true,                // skip fusermount; exercise only the terminal wait
		done:       make(chan struct{}), // never closed: the ltfs index write never finishes
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := m.Unmount(ctx)
	require.Error(t, err,
		"Unmount must return when its context is cancelled instead of waiting forever for the index write")
	assert.ErrorIs(t, err, context.Canceled)
}

// TestUnmountFusermountFailureBenignAfterProcessExit proves the second half of
// AC1: when fusermount -u fails but the supervised ltfs process has already
// exited (the mount is already gone), Unmount treats the detach failure as
// benign and reports the recorded index-write result instead of a spurious
// "not in mtab" error. Uses a real fusermount against an unmounted path, so it
// skips cleanly where fusermount is unavailable.
func TestUnmountFusermountFailureBenignAfterProcessExit(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("fusermount"); err != nil {
		t.Skipf("fusermount not on PATH: %v", err)
	}

	// detached=false forces the fusermount -u attempt; the mountpoint is not
	// mounted, so fusermount fails. Because done is closed, that failure must be
	// swallowed and the (nil) index-write result reported.
	m := &Mount{
		mountpoint: t.TempDir(),
		detached:   false,
		done:       closedDone(),
		waitErr:    nil,
	}

	require.NoError(t, m.Unmount(t.Context()),
		"a fusermount failure after the process exited must be treated as benign")
}
