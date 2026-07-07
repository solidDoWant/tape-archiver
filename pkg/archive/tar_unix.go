//go:build unix

package archive

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// hardlinkID reports whether info names a hardlinked regular file (link count
// greater than one) and, if so, returns its {device, inode} identity. Callers
// use the identity to store the file once and reference later sightings as tar
// hardlink entries. It returns ok=false when the platform stat data is missing
// or the file has a single link.
func hardlinkID(info fs.FileInfo) (id fileID, ok bool) {
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !statOK || stat.Nlink <= 1 {
		return fileID{}, false
	}

	return fileID{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, true
}

// sparseDataRegions inspects an open regular file and, when it is sparse (fewer
// blocks allocated than its logical size implies), returns its allocated data
// extents so the holes can be omitted from the archive. size is the file's
// logical size. sparse is false for ordinary fully-allocated files, which are
// archived by a plain contiguous copy. A wholly-empty (all-hole) file reports
// sparse=true with no regions.
func sparseDataRegions(file *os.File, size int64) (regions []sparseRegion, sparse bool, err error) {
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || size == 0 || int64(stat.Blocks)*512 >= size {
		// No stat data, an empty file, or every logical byte is backed by an
		// allocated block: nothing to gain from sparse encoding.
		return nil, false, nil
	}

	regions, err = seekDataRegions(file, size)
	if err != nil {
		return nil, false, err
	}

	return regions, true, nil
}

// seekDataRegions walks a file's data/hole map with SEEK_DATA/SEEK_HOLE and
// returns its allocated data extents in order.
func seekDataRegions(file *os.File, size int64) ([]sparseRegion, error) {
	var regions []sparseRegion

	offset := int64(0)
	for offset < size {
		dataStart, err := file.Seek(offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO means there is no more data past offset; the rest is a hole.
			if errors.Is(err, syscall.ENXIO) {
				break
			}

			return nil, err
		}

		holeStart, err := file.Seek(dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, err
		}

		if holeStart > size {
			holeStart = size
		}

		regions = append(regions, sparseRegion{offset: dataStart, length: holeStart - dataStart})
		offset = holeStart
	}

	return regions, nil
}
