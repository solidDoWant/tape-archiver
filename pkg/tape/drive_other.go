//go:build !linux

package tape

import (
	"context"
	"errors"
)

// IsBlank is only implemented on Linux, where the blank check is issued as a
// raw SCSI READ(6) via the SG_IO ioctl (see drive_linux.go). The data worker
// runs exclusively on Linux; this stub exists so the package still builds on
// other platforms for tooling and editors.
func (d *Drive) IsBlank(_ context.Context) (bool, error) {
	return false, errors.New("tape: IsBlank is only supported on Linux (SG_IO)")
}
