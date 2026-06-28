package archive

import (
	"context"
	"io"
)

// ctxReader wraps an io.Reader so that each Read first observes context
// cancellation. It gives io.Copy / io.CopyN cancellation granularity of a
// single read buffer even within one large file or slice.
//
// This matters because the snapshots being archived routinely contain
// multi-gigabyte files (e.g. media datasets), and the prepare stages run as
// Temporal activities that must respond promptly to cancellation or worker
// shutdown rather than blocking until a single large copy finishes.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}

	return cr.r.Read(p)
}
