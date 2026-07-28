package mhvtl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// inventory builds an Inventory with the given slot occupancy for tests.
func inventory(slots ...tape.StorageElement) tape.Inventory {
	return tape.Inventory{Slots: slots}
}

// materialiseMedia does to homeDir what a working make_vtl_media does: give every
// barcode in source a volume directory holding the mam file mhvtl identifies and
// mounts a cartridge by. Stubbing the media step without this models a
// make_vtl_media that reports success and creates nothing — the real defect, not
// the normal case.
func materialiseMedia(t *testing.T, homeDir string, source tape.Inventory) {
	t.Helper()

	for _, barcode := range occupiedBarcodes(source) {
		dir := filepath.Join(homeDir, barcode)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, mamFile), nil, 0o644))
	}
}

// makeMediaFor returns a makeMedia stub that materialises source's volumes into
// whichever home Mirror resolves.
func makeMediaFor(t *testing.T, source tape.Inventory) func(context.Context, string, string) error {
	t.Helper()

	return func(_ context.Context, _, homeDir string) error {
		materialiseMedia(t, homeDir, source)

		return nil
	}
}

// TestRenderLibraryContentsMirrorsOccupancy is the core fidelity contract: the
// rendered file must reproduce the source library's slot occupancy exactly —
// occupied slots keep their barcode, empty slots stay empty — because that is
// what decides whether a run finds the blanks it was configured to use.
func TestRenderLibraryContentsMirrorsOccupancy(t *testing.T) {
	t.Parallel()

	got := RenderLibraryContents(inventory(
		tape.StorageElement{Address: 1, Full: true, Barcode: "REAL01L6"},
		tape.StorageElement{Address: 2, Full: false},
		tape.StorageElement{Address: 3, Full: true, Barcode: "REAL03L6"},
	))

	assert.Contains(t, got, "Slot 1: REAL01L6\n")
	assert.Contains(t, got, "Slot 2:\n")
	assert.Contains(t, got, "Slot 3: REAL03L6\n")

	// Drives and the I/O station start empty — a rehearsal begins with every tape
	// at rest, and the Load phase is what moves them.
	assert.Contains(t, got, "Drive 1:\n")
	assert.Contains(t, got, "Drive 2:\n")
	assert.NotContains(t, got, "Drive 1: ")
}

// TestRenderLibraryContentsSortsSlots checks slots are emitted in ascending
// address order regardless of the order the changer reported them.
func TestRenderLibraryContentsSortsSlots(t *testing.T) {
	t.Parallel()

	got := RenderLibraryContents(inventory(
		tape.StorageElement{Address: 3, Full: true, Barcode: "C"},
		tape.StorageElement{Address: 1, Full: true, Barcode: "A"},
		tape.StorageElement{Address: 2, Full: true, Barcode: "B"},
	))

	first := strings.Index(got, "Slot 1: A")
	second := strings.Index(got, "Slot 2: B")
	third := strings.Index(got, "Slot 3: C")

	require.NotEqual(t, -1, first)
	assert.Less(t, first, second)
	assert.Less(t, second, third)
}

// TestRenderLibraryContentsTreatsBarcodelessSlotAsEmpty checks a slot flagged
// full but carrying no volume tag renders empty rather than as a malformed entry
// mhvtl would fail to parse.
func TestRenderLibraryContentsTreatsBarcodelessSlotAsEmpty(t *testing.T) {
	t.Parallel()

	got := RenderLibraryContents(inventory(tape.StorageElement{Address: 4, Full: true, Barcode: ""}))

	assert.Contains(t, got, "Slot 4:\n")
	assert.NotContains(t, got, "Slot 4: \n")
}

// TestMirrorWritesContentsRegeneratesMediaAndSignals is the end-to-end contract
// for a reprovision: it writes the mirrored contents to the library's own
// library_contents file, regenerates the media, and SIGHUPs the daemon so
// vtllibrary re-reads it — in that order, since signalling before the file is
// written would reload the previous contents.
func TestMirrorWritesContentsRegeneratesMediaAndSignals(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	homeDir := t.TempDir()

	var (
		order       []string
		contentsAt  string
		signaledPID int
	)

	source := inventory(
		tape.StorageElement{Address: 1, Full: true, Barcode: "REAL01L6"},
		tape.StorageElement{Address: 2, Full: false},
	)

	r := New(WithConfigDir(configDir), WithHomeDir(homeDir), WithLibraryID(42))
	r.makeMedia = func(ctx context.Context, gotConfig, gotHome string) error {
		order = append(order, "makeMedia")

		assert.Equal(t, configDir, gotConfig)
		assert.Equal(t, homeDir, gotHome)
		// The contents must already be on disk: make_vtl_media creates media from
		// whatever library_contents names.
		contentsAt = "written-before-makeMedia"

		return makeMediaFor(t, source)(ctx, gotConfig, gotHome)
	}
	r.findDaemon = func(id int) (int, error) {
		assert.Equal(t, 42, id, "must look up the daemon for the configured library ID")

		return 4321, nil
	}
	r.signal = func(pid int) error {
		order = append(order, "signal")
		signaledPID = pid

		return nil
	}

	err := r.Mirror(t.Context(), source)
	require.NoError(t, err)

	assert.Equal(t, []string{"makeMedia", "signal"}, order,
		"media must be regenerated before the daemon is told to re-read")
	assert.Equal(t, 4321, signaledPID)
	assert.Equal(t, "written-before-makeMedia", contentsAt)

	// The contents landed at library_contents.<id>, which is what vtllibrary reads.
	written, err := os.ReadFile(filepath.Join(configDir, "library_contents.42"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "Slot 1: REAL01L6")
	assert.Contains(t, string(written), "Slot 2:\n")
}

// TestMirrorSurfacesDaemonLookupFailure checks a missing vtllibrary is reported
// rather than silently skipped — a dry run that believes it mirrored but did not
// is exactly the false confidence this feature exists to remove.
func TestMirrorSurfacesDaemonLookupFailure(t *testing.T) {
	t.Parallel()

	source := inventory(tape.StorageElement{Address: 1, Full: true, Barcode: "X"})

	r := New(WithConfigDir(t.TempDir()), WithHomeDir(t.TempDir()))
	r.makeMedia = makeMediaFor(t, source)
	r.findDaemon = func(int) (int, error) { return 0, ErrDaemonNotFound }

	err := r.Mirror(t.Context(), source)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDaemonNotFound))
}

// writeConf writes an mhvtl.conf containing body into dir and returns dir.
func writeConf(t *testing.T, dir, body string) string {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, mhvtlConfFile), []byte(body), 0o644))

	return dir
}

// TestMirrorTakesTheMediaHomeFromMhvtlConf is the contract this exists for: media
// go where the host says mhvtl keeps them, not where the code was compiled to
// guess. A worker that guesses creates a full set of volumes somewhere vtllibrary
// is not looking, reports success, and leaves the run to die later at Load.
func TestMirrorTakesTheMediaHomeFromMhvtlConf(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configDir := writeConf(t, t.TempDir(),
		"# mhvtl settings\nCAPACITY=500\nMHVTL_HOME_PATH="+homeDir+"\n")

	source := inventory(tape.StorageElement{Address: 1, Full: true, Barcode: "X"})
	gotHome := ""

	r := New(WithConfigDir(configDir))
	r.makeMedia = func(ctx context.Context, configDir, home string) error {
		gotHome = home

		return makeMediaFor(t, source)(ctx, configDir, home)
	}
	r.findDaemon = func(int) (int, error) { return 1, nil }
	r.signal = func(int) error { return nil }

	require.NoError(t, r.Mirror(t.Context(), source))

	assert.Equal(t, homeDir, gotHome)
}

// TestMirrorPrefersAnExplicitHomeDir checks WithHomeDir still wins, so a caller
// that knows better than mhvtl.conf is not overridden by it.
func TestMirrorPrefersAnExplicitHomeDir(t *testing.T) {
	t.Parallel()

	pinned := t.TempDir()
	configDir := writeConf(t, t.TempDir(), "MHVTL_HOME_PATH="+t.TempDir()+"\n")

	source := inventory(tape.StorageElement{Address: 1, Full: true, Barcode: "X"})
	gotHome := ""

	r := New(WithConfigDir(configDir), WithHomeDir(pinned))
	r.makeMedia = func(ctx context.Context, configDir, home string) error {
		gotHome = home

		return makeMediaFor(t, source)(ctx, configDir, home)
	}
	r.findDaemon = func(int) (int, error) { return 1, nil }
	r.signal = func(int) error { return nil }

	require.NoError(t, r.Mirror(t.Context(), source))

	assert.Equal(t, pinned, gotHome)
}

// TestMirrorRefusesAnUnusableMediaHome checks every way the home can be unusable
// stops the reprovision before it writes anything. Failing loudly here is the
// whole point: the alternative is a mirror that claims success against media that
// are not where the library will look for them.
func TestMirrorRefusesAnUnusableMediaHome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the config dir and the WithHomeDir override ("" for none).
		setup     func(t *testing.T) (configDir, homeDir string)
		wantError string
	}{
		{
			name: "no mhvtl.conf at all",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return t.TempDir(), ""
			},
			wantError: "read",
		},
		{
			name: "mhvtl.conf declares no media home",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(), "CAPACITY=500\n"), ""
			},
			wantError: "declares no MHVTL_HOME_PATH",
		},
		{
			name: "the declared media home does not exist",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(),
					"MHVTL_HOME_PATH="+filepath.Join(t.TempDir(), "absent")+"\n"), ""
			},
			wantError: "stat the mhvtl media home",
		},
		{
			name: "the declared media home is a file",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				file := filepath.Join(t.TempDir(), "not-a-dir")
				require.NoError(t, os.WriteFile(file, nil, 0o644))

				return writeConf(t, t.TempDir(), "MHVTL_HOME_PATH="+file+"\n"), ""
			},
			wantError: "is not a directory",
		},
		{
			name: "the declared media home needs shell expansion",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(), "MHVTL_HOME_PATH=$HOME/mhvtl\n"), ""
			},
			wantError: "does not expand",
		},
		{
			name: "an explicit media home that does not exist",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return t.TempDir(), filepath.Join(t.TempDir(), "absent")
			},
			wantError: "stat the mhvtl media home",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configDir, homeDir := test.setup(t)

			opts := []Option{WithConfigDir(configDir)}
			if homeDir != "" {
				opts = append(opts, WithHomeDir(homeDir))
			}

			mediaMade, signalled := false, false

			r := New(opts...)
			r.makeMedia = func(context.Context, string, string) error { mediaMade = true; return nil }
			r.findDaemon = func(int) (int, error) { return 1, nil }
			r.signal = func(int) error { signalled = true; return nil }

			err := r.Mirror(t.Context(), inventory(
				tape.StorageElement{Address: 1, Full: true, Barcode: "X"}))

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantError)
			assert.False(t, mediaMade, "no media may be created against an unusable home")
			assert.False(t, signalled, "the daemon must not be signalled")

			// Nothing may be written either: an aborted mirror leaves the library's
			// existing contents alone rather than half-rewritten.
			_, err = os.Stat(filepath.Join(configDir, "library_contents.10"))
			assert.True(t, os.IsNotExist(err), "library_contents must not have been rewritten")
		})
	}
}

// TestReadHomePathParsesConfLikeShellSourcingWould checks the parse matches how
// mhvtl itself reads the file — it sources it — so a conf that works for
// make_vtl_media resolves to the same path here.
func TestReadHomePathParsesConfLikeShellSourcingWould(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		conf string
		want string
	}{
		{
			name: "a plain assignment",
			conf: "MHVTL_HOME_PATH=/srv/mhvtl\n",
			want: "/srv/mhvtl",
		},
		{
			name: "indented and surrounded by other settings",
			conf: "CAPACITY=500\n  MHVTL_HOME_PATH=/srv/mhvtl\nVERBOSE=0\n",
			want: "/srv/mhvtl",
		},
		{
			name: "a quoted value",
			conf: "MHVTL_HOME_PATH=\"/srv/mhvtl\"\n",
			want: "/srv/mhvtl",
		},
		{
			name: "a trailing comment",
			conf: "MHVTL_HOME_PATH=/srv/mhvtl # on the bulk pool\n",
			want: "/srv/mhvtl",
		},
		{
			name: "a commented-out assignment is ignored",
			conf: "#MHVTL_HOME_PATH=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n",
			want: "/srv/mhvtl",
		},
		{
			name: "the last assignment wins, as sourcing would leave it",
			conf: "MHVTL_HOME_PATH=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n",
			want: "/srv/mhvtl",
		},
		{
			name: "an exported assignment",
			conf: "export MHVTL_HOME_PATH=/srv/mhvtl\n",
			want: "/srv/mhvtl",
		},
		{
			name: "a similarly named setting is not mistaken for it",
			conf: "MHVTL_HOME_PATH_OLD=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n",
			want: "/srv/mhvtl",
		},
		{
			name: "a file with no trailing newline",
			conf: "MHVTL_HOME_PATH=/srv/mhvtl",
			want: "/srv/mhvtl",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := readHomePath(filepath.Join(writeConf(t, t.TempDir(), test.conf), mhvtlConfFile))

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestMirrorSurfacesMediaFailure checks a failed make_vtl_media aborts before the
// daemon is signalled, so the library is never reloaded onto media that does not
// exist.
func TestMirrorSurfacesMediaFailure(t *testing.T) {
	t.Parallel()

	signalled := false

	r := New(WithConfigDir(t.TempDir()), WithHomeDir(t.TempDir()))
	r.makeMedia = func(context.Context, string, string) error { return errors.New("boom") }
	r.findDaemon = func(int) (int, error) { return 1, nil }
	r.signal = func(int) error { signalled = true; return nil }

	err := r.Mirror(t.Context(), inventory(tape.StorageElement{Address: 1, Full: true, Barcode: "X"}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "regenerate mhvtl media")
	assert.False(t, signalled, "the daemon must not be signalled after a media failure")
}

// TestMirrorRefusesBarcodesWithNoMedia is the contract that closes the gap a
// zero exit status left open: make_vtl_media can report success and create
// nothing (no awk on its PATH, and both of its loops iterate over an empty list),
// and mhvtl cannot tell the difference until a tape is mounted — vtllibrary
// derives element status from library_contents alone, so the changer cheerfully
// reports tapes that will fail to load. The mirror must not hand that library on.
func TestMirrorRefusesBarcodesWithNoMedia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// made are the barcodes make_vtl_media actually materialised.
		made []string
		// wantMissing are the barcodes the error must name, "" for none.
		wantMissing []string
	}{
		{
			name:        "no media at all",
			made:        nil,
			wantMissing: []string{"AAA01L6", "BBB02L6"},
		},
		{
			name:        "media for only some barcodes",
			made:        []string{"BBB02L6"},
			wantMissing: []string{"AAA01L6"},
		},
		{
			name: "media for every barcode",
			made: []string{"AAA01L6", "BBB02L6"},
		},
	}

	source := inventory(
		tape.StorageElement{Address: 1, Full: true, Barcode: "AAA01L6"},
		tape.StorageElement{Address: 2, Full: false},
		tape.StorageElement{Address: 3, Full: true, Barcode: "BBB02L6"},
	)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			homeDir := t.TempDir()
			signalled := false

			r := New(WithConfigDir(t.TempDir()), WithHomeDir(homeDir))
			r.makeMedia = func(context.Context, string, string) error {
				made := make([]tape.StorageElement, 0, len(test.made))
				for index, barcode := range test.made {
					made = append(made, tape.StorageElement{
						Address: index + 1, Full: true, Barcode: tape.Barcode(barcode),
					})
				}

				materialiseMedia(t, homeDir, inventory(made...))

				return nil
			}
			r.findDaemon = func(int) (int, error) { return 1, nil }
			r.signal = func(int) error { signalled = true; return nil }

			err := r.Mirror(t.Context(), source)

			if test.wantMissing == nil {
				require.NoError(t, err)
				assert.True(t, signalled, "a fully materialised mirror must reload the daemon")

				return
			}

			require.Error(t, err)
			assert.False(t, signalled,
				"the daemon must not be reloaded onto barcodes with no backing media")

			for _, barcode := range test.wantMissing {
				assert.Contains(t, err.Error(), barcode,
					"the error must name the barcode an operator has to go looking for")
			}
		})
	}
}
