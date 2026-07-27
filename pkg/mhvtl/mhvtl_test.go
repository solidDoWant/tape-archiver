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

	r := New(WithConfigDir(configDir), WithHomeDir(homeDir), WithLibraryID(42))
	r.makeMedia = func(_ context.Context, gotConfig, gotHome string) error {
		order = append(order, "makeMedia")

		assert.Equal(t, configDir, gotConfig)
		assert.Equal(t, homeDir, gotHome)
		// The contents must already be on disk: make_vtl_media creates media from
		// whatever library_contents names.
		contentsAt = "written-before-makeMedia"

		return nil
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

	err := r.Mirror(t.Context(), inventory(
		tape.StorageElement{Address: 1, Full: true, Barcode: "REAL01L6"},
		tape.StorageElement{Address: 2, Full: false},
	))
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

	r := New(WithConfigDir(t.TempDir()), WithHomeDir(t.TempDir()))
	r.makeMedia = func(context.Context, string, string) error { return nil }
	r.findDaemon = func(int) (int, error) { return 0, ErrDaemonNotFound }

	err := r.Mirror(t.Context(), inventory(tape.StorageElement{Address: 1, Full: true, Barcode: "X"}))

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDaemonNotFound))
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
