//go:build !linux

package tape

import (
	"context"
	"errors"
)

// errRawReadUnsupported is returned by the RawReader stubs off Linux, where the
// SG_IO LOCATE/READ path is not built (see rawread_linux.go). The data worker
// runs exclusively on Linux; these stubs exist so the package still builds on
// other platforms for tooling and editors.
var errRawReadUnsupported = errors.New("tape: raw block read is only supported on Linux (SG_IO)")

// RawReader is the raw-block reader; its behavior is Linux-only (see
// rawread_linux.go). This stub type keeps the package building elsewhere.
type RawReader struct{}

// OpenRawReader is only implemented on Linux.
func (d *Drive) OpenRawReader() (*RawReader, error) {
	return nil, errRawReadUnsupported
}

// Close is a no-op on the stub.
func (r *RawReader) Close() error { return nil }

// Locate is only implemented on Linux.
func (r *RawReader) Locate(_ context.Context, _ string, _ uint64) error {
	return errRawReadUnsupported
}

// ReadBlock is only implemented on Linux.
func (r *RawReader) ReadBlock(_ context.Context) ([]byte, error) {
	return nil, errRawReadUnsupported
}
