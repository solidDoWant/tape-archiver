//go:build !linux

package tape

import (
	"context"
	"errors"
)

// The changer issues its SCSI commands via the Linux SG_IO ioctl (see
// changer_linux.go). The data worker runs exclusively on Linux; these stubs exist
// so the package still builds on other platforms for tooling and editors.

var errChangerUnsupported = errors.New("tape: Changer is only supported on Linux (SG_IO)")

// Inventory is only implemented on Linux.
func (c *Changer) Inventory(_ context.Context) (Inventory, error) {
	return Inventory{}, errChangerUnsupported
}

// Load is only implemented on Linux.
func (c *Changer) Load(_ context.Context, _, _ int) error {
	return errChangerUnsupported
}

// Unload is only implemented on Linux.
func (c *Changer) Unload(_ context.Context, _, _ int) error {
	return errChangerUnsupported
}

// Transfer is only implemented on Linux.
func (c *Changer) Transfer(_ context.Context, _, _ int) error {
	return errChangerUnsupported
}
