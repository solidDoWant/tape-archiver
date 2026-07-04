//go:build integration

package testutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestMhvtlChangerEnumeration verifies that the virtual tape library presents
// the expected topology through the same SG_IO changer code path used for real
// hardware.
func TestMhvtlChangerEnumeration(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "changer inventory")

	assert.Len(t, inv.Drives, 2, "expected 2 data transfer elements (drives)")
	assert.GreaterOrEqual(t, len(inv.Slots), 47, "expected at least 47 storage slots")
	assert.GreaterOrEqual(t, len(inv.IOSlots), 3, "expected at least 3 import/export slots")
	assert.True(t, inv.IOAccessReported, "READ ELEMENT STATUS must report the import/export ACCESS bit")
}
