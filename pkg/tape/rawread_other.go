//go:build !linux

package tape

import (
	"context"
	"errors"
)

// errRawReadUnsupported is returned off Linux; the stubs only keep the package
// building elsewhere.
var errRawReadUnsupported = errors.New("tape: raw block read is only supported on Linux (SG_IO)")

// RawReader is the raw-block reader; it is Linux-only.
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
