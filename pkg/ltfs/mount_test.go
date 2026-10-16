package ltfs

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnescapeMountinfo covers mountinfo's octal unescaping.
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

// TestMountpointInUse checks the mountinfo scan, which never stats the path so it
// can detect an orphaned ENOTCONN mount.
func TestMountpointInUse(t *testing.T) {
	t.Parallel()

	inUse, err := mountpointInUse("/")
	require.NoError(t, err)
	assert.True(t, inUse, "/ must be reported as a mount point")

	inUse, err = mountpointInUse(t.TempDir())
	require.NoError(t, err)
	assert.False(t, inUse, "a fresh temp dir is not a mount point")

	// A missing final component must not error (the parent is resolved instead).
	inUse, err = mountpointInUse(t.TempDir() + "/does-not-exist")
	require.NoError(t, err)
	assert.False(t, inUse, "a non-existent path is not a mount point")
}

// closedDone stands in for an ltfs process that has already exited.
func closedDone() chan struct{} {
	done := make(chan struct{})
	close(done)

	return done
}

// TestUnmountIdempotentAfterDetach: once detached, a retry must not re-run
// fusermount -u, which would fail on the unmounted path.
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

// TestWaitForMountExitError pins two regressions from run 019f7dda: a nil
// waitErr must not render as "%!w(<nil>)", and ltfs stderr must keep its
// newlines.
func TestWaitForMountExitError(t *testing.T) {
	t.Parallel()

	const ltfsStderr = "LTFS9015W Setting the locale to 'en_US.UTF-8'.\n" +
		"7427 LTFS14000I LTFS starting, LTFS version 2.4.8.4 (Prelim), log level 2."

	const indentedOutput = "  LTFS9015W Setting the locale to 'en_US.UTF-8'.\n" +
		"  7427 LTFS14000I LTFS starting, LTFS version 2.4.8.4 (Prelim), log level 2."

	exitErr := errors.New("exit status 1")

	tests := []struct {
		name       string
		waitErr    error
		stderr     string
		wantSubstr []string
	}{
		{
			// The run-019f7dda regression: ltfs exited 0 without mounting.
			name:       "clean exit without mounting",
			waitErr:    nil,
			stderr:     ltfsStderr,
			wantSubstr: []string{"status 0", "before the mount became ready", "ltfs output:", indentedOutput},
		},
		{
			name:       "non-nil exit error is wrapped",
			waitErr:    exitErr,
			stderr:     ltfsStderr,
			wantSubstr: []string{"exit status 1", "ltfs output:", indentedOutput},
		},
		{
			name:       "no stderr omits the output block",
			waitErr:    nil,
			stderr:     "",
			wantSubstr: []string{"status 0", "before the mount became ready"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			m := &Mount{
				mountpoint: t.TempDir(), // exists but is not a mount point
				stderr:     bytes.NewBufferString(test.stderr),
				done:       closedDone(), // the supervised ltfs process has already exited
				waitErr:    test.waitErr,
			}

			err := m.waitForMount(t.Context())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "%!w",
				"a nil waitErr must not be wrapped with %%w")

			for _, want := range test.wantSubstr {
				assert.Contains(t, err.Error(), want)
			}

			if test.stderr == "" {
				assert.NotContains(t, err.Error(), "ltfs output:")
			}

			if test.waitErr != nil {
				assert.ErrorIs(t, err, test.waitErr, "the ltfs exit error must remain unwrappable")
			}
		})
	}
}

// TestUnmountReturnsWhenContextCancelled: the index-write wait honours ctx, so
// Teardown can fall through to Kill instead of hanging (#223).
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

// TestUnmountFusermountFailureBenignAfterProcessExit: a fusermount failure after
// ltfs has exited is benign and the index-write result is reported.
func TestUnmountFusermountFailureBenignAfterProcessExit(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("fusermount"); err != nil {
		t.Skipf("fusermount not on PATH: %v", err)
	}

	// fusermount fails on the unmounted path; done is closed, so it is swallowed.
	m := &Mount{
		mountpoint: t.TempDir(),
		detached:   false,
		done:       closedDone(),
		waitErr:    nil,
	}

	require.NoError(t, m.Unmount(t.Context()),
		"a fusermount failure after the process exited must be treated as benign")
}

// startSupervised starts a stand-in ltfs process supervised like Volume.Mount's.
func startSupervised(t *testing.T, name string, args ...string) *Mount {
	t.Helper()

	cmd := exec.Command(name, args...)
	require.NoError(t, cmd.Start())

	m := &Mount{
		device:     "/dev/null", // opens, but rejects SG_IO
		mountpoint: t.TempDir(), // exists but is not a mount point
		cmd:        cmd,
		stderr:     &bytes.Buffer{},
		done:       make(chan struct{}),
	}

	go func() {
		m.waitErr = cmd.Wait()
		close(m.done)
	}()

	return m
}

// TestAbortKillsThenReleasesDrive: Abort must kill ltfs and still attempt the
// drive release, surfacing its failure; an already-exited ltfs is not an error
// (#442).
func TestAbortKillsThenReleasesDrive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  []string
	}{
		{name: "live ltfs is killed", cmd: []string{"sleep", "600"}},
		{name: "already-exited ltfs", cmd: []string{"true"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			m := startSupervised(t, test.cmd[0], test.cmd[1:]...)
			if test.cmd[0] == "true" {
				<-m.done
			}

			err := m.Abort(t.Context())

			select {
			case <-m.done:
			default:
				t.Fatal("Abort returned before ltfs exited")
			}

			// The release ran and its failure is reported, naming the drive.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "release drive /dev/null")
			assert.Contains(t, err.Error(), "ALLOW MEDIUM REMOVAL")
			assert.NotContains(t, err.Error(), "kill ltfs", "an exited process is not a kill failure")
		})
	}
}
