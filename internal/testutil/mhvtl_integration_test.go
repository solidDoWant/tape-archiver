//go:build integration

package testutil_test

import (
	"strings"
	"testing"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMhvtlChangerEnumeration verifies that the virtual tape library presents
// the expected topology through the same mtx code path used for real hardware.
func TestMhvtlChangerEnumeration(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changerDev := testutil.ChangerDev(t)

	out, err := testutil.MtxCommand(t.Context(), changerDev, "status").Output()
	require.NoError(t, err, "mtx -f %s status failed", changerDev)

	drives, storageSlots, ieSlots := parseMtxStatus(string(out))

	assert.Equal(t, 2, drives, "expected 2 data transfer elements (drives)")
	assert.GreaterOrEqual(t, storageSlots, 47, "expected at least 47 storage slots")
	assert.GreaterOrEqual(t, ieSlots, 3, "expected at least 3 import/export slots")
}

// parseMtxStatus counts drives, storage slots, and I/E slots from mtx output.
// Example lines:
//
//	Data Transfer Element 0:Empty
//	      Storage Element 1:Full :VolumeTag=TA0001L6
//	      Storage Element 48 IMPORT/EXPORT:Empty
func parseMtxStatus(output string) (drives, storageSlots, ieSlots int) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Data Transfer Element "):
			drives++
		case strings.Contains(line, "IMPORT/EXPORT"):
			ieSlots++
		case strings.HasPrefix(line, "Storage Element "):
			storageSlots++
		}
	}

	return drives, storageSlots, ieSlots
}
