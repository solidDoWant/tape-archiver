//go:build !linux

package tape

import (
	"context"
	"errors"
)

// SGDevice is only implemented on Linux.
func (d *Drive) SGDevice() (string, error) {
	return "", errors.New("tape: SGDevice is only supported on Linux (sysfs SCSI address resolution)")
}

// IsBlank is only implemented on Linux.
func (d *Drive) IsBlank(_ context.Context) (bool, error) {
	return false, errors.New("tape: IsBlank is only supported on Linux (SG_IO)")
}

// AllowMediumRemoval is only implemented on Linux.
func AllowMediumRemoval(context.Context, string) error {
	return errors.New("tape: ALLOW MEDIUM REMOVAL is only supported on Linux (SG_IO)")
}

func testUnitReady(context.Context, string) error {
	return errors.New("tape: TEST UNIT READY is only supported on Linux (SG_IO)")
}
