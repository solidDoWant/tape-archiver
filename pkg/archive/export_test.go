package archive

import (
	"context"
	"io"
)

// ReadAhead sets the read-ahead bounds for TarWith; Workers 0 is the serial
// reference path.
type ReadAhead struct {
	Workers int
	MaxFile int64
	Memory  int64
	Window  int
}

// DefaultReadAhead is the production read-ahead configuration.
var DefaultReadAhead = ReadAhead{
	Workers: defaultReadAhead.workers,
	MaxFile: defaultReadAhead.maxFile,
	Memory:  defaultReadAhead.memory,
	Window:  defaultReadAhead.window,
}

func (r ReadAhead) internal() readAhead {
	return readAhead{workers: r.Workers, maxFile: r.MaxFile, memory: r.Memory, window: r.Window}
}

// TarWith is Tar with explicit read-ahead bounds.
func TarWith(ctx context.Context, w io.Writer, srcDir string, config ReadAhead) error {
	return tarWith(ctx, w, srcDir, config.internal())
}

// TarMembersWith is TarMembers with explicit read-ahead bounds.
func TarMembersWith(ctx context.Context, w io.Writer, members []Member, config ReadAhead) error {
	return tarMembersWith(ctx, w, members, config.internal())
}
