package archive

import (
	"context"
	"io"
)

// ctxReader checks ctx on each Read so copying a multi-gigabyte file still
// responds promptly to activity cancellation.
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
