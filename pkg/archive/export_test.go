package archive

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
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

// SparseRegion is one data extent passed to WriteSparseHeader.
type SparseRegion struct {
	Offset int64
	Length int64
}

// WriteSparseHeader writes a sparse member's PAX header, data header, and
// sparse map for header and regions, without the data itself.
func WriteSparseHeader(w io.Writer, header *tar.Header, regions []SparseRegion) error {
	internal := make([]sparseRegion, 0, len(regions))
	for _, region := range regions {
		internal = append(internal, sparseRegion{offset: region.Offset, length: region.Length})
	}

	_, err := newTarWriter(w, readAhead{}).writeSparseHeader(header, internal)

	return err
}

// WriteSparseFile archives file as the only member, a sparse entry carrying
// header's metadata, and ends the archive.
func WriteSparseFile(ctx context.Context, w io.Writer, header *tar.Header, file *os.File) error {
	regions, sparse, err := sparseDataRegions(file, header.Size)
	if err != nil {
		return err
	}

	if !sparse {
		return errors.New("file is not sparse")
	}

	writer := newTarWriter(w, readAhead{})
	if err := writer.writeSparseEntry(ctx, header, file, regions); err != nil {
		return err
	}

	return writer.tw.Close()
}
