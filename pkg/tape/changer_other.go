//go:build !linux

package tape

import (
	"context"
	"errors"
)

// Linux-only (SG_IO); these stubs keep the package building elsewhere.

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
