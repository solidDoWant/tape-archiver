//go:build linux

package tape

import (
	"context"
	"os"
	"time"
	"unsafe"
)

const (
	opcodePreventAllowMediumRemoval = 0x1E // PREVENT ALLOW MEDIUM REMOVAL

	preventAllowTimeout = 30 * time.Second
)

// AllowMediumRemoval clears the drive's medium-removal lock (PREVENT ALLOW
// MEDIUM REMOVAL, PREVENT=00b) via its sg node. ltfs sets the lock at mount and
// clears it on a clean exit; a killed ltfs leaves it set and the changer then
// refuses to unload the tape (ASC 53/02, #442). Idempotent and harmless when no
// lock is set.
//
// Call it only once nothing is using the drive: the lock is what stops the
// changer pulling a tape out from under a live LTFS mount.
func AllowMediumRemoval(ctx context.Context, sgDevice string) error {
	return withSGFile(sgDevice, func(f *os.File) error {
		cdb := preventAllowCDB(false)
		sense := make([]byte, senseBufferLen)

		hdr := sgIOHdr{
			interfaceID:    'S',
			dxferDirection: sgDxferNone,
			cmdLen:         uint8(len(cdb)),
			mxSbLen:        uint8(len(sense)),
			cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
			sbp:            uintptr(unsafe.Pointer(&sense[0])),
			timeout:        timeoutMs(ctx, preventAllowTimeout),
		}

		return sgCommand(ctx, f, &hdr, cdb, sense, nil, "ALLOW MEDIUM REMOVAL")
	})
}

// preventAllowCDB builds a PREVENT ALLOW MEDIUM REMOVAL CDB (SPC-4); the
// PREVENT field is byte 4, bits 1:0.
func preventAllowCDB(prevent bool) []byte {
	cdb := []byte{opcodePreventAllowMediumRemoval, 0, 0, 0, 0, 0}
	if prevent {
		cdb[4] = 0x01
	}

	return cdb
}
