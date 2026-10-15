//go:build !linux

package tape

import (
	"context"
	"errors"
)

// Linux-only (SG_IO); these stubs keep the package building elsewhere.

var errInquiryUnsupported = errors.New("tape: Inquire is only supported on Linux (SG_IO)")

// Inquire is only implemented on Linux.
func (d *Drive) Inquire(_ context.Context) (DeviceInfo, error) {
	return DeviceInfo{}, errInquiryUnsupported
}

// Inquire is only implemented on Linux.
func (c *Changer) Inquire(_ context.Context) (DeviceInfo, error) {
	return DeviceInfo{}, errInquiryUnsupported
}
