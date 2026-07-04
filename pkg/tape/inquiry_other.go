//go:build !linux

package tape

import (
	"context"
	"errors"
)

// INQUIRY is issued via the Linux SG_IO ioctl (see inquiry_linux.go). The data
// worker runs exclusively on Linux; these stubs exist so the package still builds
// on other platforms for tooling and editors.

var errInquiryUnsupported = errors.New("tape: Inquire is only supported on Linux (SG_IO)")

// Inquire is only implemented on Linux.
func (d *Drive) Inquire(_ context.Context) (DeviceInfo, error) {
	return DeviceInfo{}, errInquiryUnsupported
}

// Inquire is only implemented on Linux.
func (c *Changer) Inquire(_ context.Context) (DeviceInfo, error) {
	return DeviceInfo{}, errInquiryUnsupported
}
