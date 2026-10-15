//go:build integration

package mhvtl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/mhvtl"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// vtllibraryPID returns the pid of the running vtllibrary daemon.
func vtllibraryPID(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && strings.HasPrefix(filepath.Base(string(cmdline)), "vtllibrary\x00") {
			return pid
		}
	}

	t.Fatal("no vtllibrary daemon running")

	return 0
}

// TestMirrorRearrangesALiveLibrary runs back-to-back mirrors against the live
// mhvtl daemons, starting with a tape left in a drive, and loads a mirrored tape
// after each one.
func TestMirrorRearrangesALiveLibrary(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	original, err := changer.Inventory(t.Context())
	require.NoError(t, err)

	// Put the library back as the rest of the suite expects it.
	t.Cleanup(func() {
		require.NoError(t, mhvtl.New().Mirror(context.WithoutCancel(t.Context()), changer, original))
	})

	pid := vtllibraryPID(t)

	// A previous run left a tape in drive 0.
	require.NoError(t, changer.Load(t.Context(), 1, 0))

	mirrors := []map[int]string{
		{1: "MIR001L6", 2: "MIR002L6", 5: "MIR005L6", 6: string(original.Slots[5].Barcode)},
		{2: "MIR002L6", 3: "MIR003L6", 4: "MIR001L6", 40: "MIR040L6"},
	}

	for round, want := range mirrors {
		t.Run(fmt.Sprintf("mirror %d", round+1), func(t *testing.T) {
			source := tape.Inventory{}

			for _, slot := range original.Slots {
				barcode := want[slot.Address]
				source.Slots = append(source.Slots, tape.StorageElement{
					Address: slot.Address, Full: barcode != "", Barcode: tape.Barcode(barcode),
				})
			}

			require.NoError(t, mhvtl.New().Mirror(t.Context(), changer, source))

			inv, err := changer.Inventory(t.Context())
			require.NoError(t, err)

			for _, slot := range inv.Slots {
				got := ""
				if slot.Full {
					got = string(slot.Barcode)
				}

				assert.Equal(t, want[slot.Address], got, "slot %d", slot.Address)
			}

			for _, drive := range inv.Drives {
				assert.False(t, drive.Loaded, "drive %d must be empty", drive.Address)
			}

			// The mirrored tapes must actually mount.
			slot := 2
			require.NoError(t, changer.Load(t.Context(), slot, 0), "load %s", want[slot])
			require.NoError(t, changer.Unload(t.Context(), slot, 0))

			assert.Equal(t, pid, vtllibraryPID(t), "vtllibrary must not have been restarted or crashed")
		})
	}
}
