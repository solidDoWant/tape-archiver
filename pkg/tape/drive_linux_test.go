//go:build linux

package tape

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterpretBlankProbe exercises the blank-probe decision half of IsBlank
// directly. mhvtl cannot produce a status-only completion, so the status gate
// that keeps a transient BUSY/RESERVATION CONFLICT from becoming a definitive
// "not blank" verdict is only reachable via unit test.
func TestInterpretBlankProbe(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		probe     blankProbe
		wantBlank bool
		assertErr require.ErrorAssertionFunc
		wantErrIs error // errors.Is target the error must match (when erroring)
	}{
		"data present is not blank": {
			probe:     blankProbe{status: statusGood, transferred: 65536},
			wantBlank: false,
			assertErr: require.NoError,
		},
		"BLANK CHECK is blank": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: senseKeyBlankCheck},
			wantBlank: true,
			assertErr: require.NoError,
		},
		"filemark is not blank": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: senseKeyNoSense, filemark: true},
			wantBlank: false,
			assertErr: require.NoError,
		},
		"unexpected sense key errors": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: 0x03, asc: 0x11, ascq: 0x00},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
		"BUSY status-only completion errors, not a false not-blank": {
			// The core bug: status-only completion, LLD leaves resid 0 so
			// transferred>0 on a genuinely blank tape. Must be an error (so the
			// ready-retry loop retries), never a definitive (false, nil).
			probe:     blankProbe{status: 0x08, transferred: 65536},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
		"RESERVATION CONFLICT status-only completion errors": {
			probe:     blankProbe{status: 0x18, transferred: 65536},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			blank, err := interpretBlankProbe(test.probe, "/dev/sg-test")

			test.assertErr(t, err)

			if err != nil {
				assert.False(t, blank, "no blank verdict may accompany an error")

				if test.wantErrIs != nil {
					assert.ErrorIs(t, err, test.wantErrIs)
				}

				return
			}

			assert.Equal(t, test.wantBlank, blank)
		})
	}
}

// TestClassEntryForDevNumber exercises the sysfs class-entry lookup that resolves
// a device node to its kernel name by device number rather than path basename —
// the core of the fix for a dry-run changer/drive reached through a /dev/mhvtl/*
// udev symlink whose basename is not a sysfs entry (issue #326). It builds a
// synthetic sysfs class tree so the logic is testable without root or real
// hardware; each entry's "dev" file mirrors the kernel's "major:minor\n" format.
func TestClassEntryForDevNumber(t *testing.T) {
	t.Parallel()

	// A class dir modelling /sys/class/scsi_changer with a renamed entry (sch2),
	// as mhvtl presents it, plus an unrelated neighbour to prove the match is by
	// number, not by iteration order.
	classDir := t.TempDir()
	writeClassEntry(t, classDir, "sch0", "86:0")
	writeClassEntry(t, classDir, "sch2", "86:2")

	// An entry with no "dev" file must be skipped, not error the whole scan.
	require.NoError(t, os.Mkdir(filepath.Join(classDir, "no-dev-file"), 0o755))

	tests := map[string]struct {
		major, minor uint32
		wantName     string
		assertErr    require.ErrorAssertionFunc
	}{
		"matches the renamed entry by number": {
			major: 86, minor: 2, wantName: "sch2", assertErr: require.NoError,
		},
		"matches a different number in the same dir": {
			major: 86, minor: 0, wantName: "sch0", assertErr: require.NoError,
		},
		"no entry with that number errors": {
			major: 86, minor: 9, assertErr: require.Error,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := classEntryForDevNumber(classDir, test.major, test.minor)

			test.assertErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.wantName, got)
		})
	}

	t.Run("missing class dir errors", func(t *testing.T) {
		t.Parallel()

		_, err := classEntryForDevNumber(filepath.Join(classDir, "does-not-exist"), 86, 2)
		require.Error(t, err)
	})
}

// writeClassEntry creates a sysfs-style class entry <classDir>/<name>/dev holding
// "<devNumber>\n", matching the kernel's uevent "dev" attribute format.
func writeClassEntry(t *testing.T, classDir, name, devNumber string) {
	t.Helper()

	dir := filepath.Join(classDir, name)
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev"), []byte(devNumber+"\n"), 0o644))
}
