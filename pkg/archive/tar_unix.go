//go:build unix

package archive

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// hardlinkID returns the {device, inode} of a file with more than one link.
func hardlinkID(info fs.FileInfo) (id fileID, ok bool) {
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !statOK || stat.Nlink <= 1 {
		return fileID{}, false
	}

	return fileID{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, true
}

// sparseDataRegions returns the data extents of a file with fewer allocated
// blocks than its size implies. An all-hole file is sparse with no regions.
func sparseDataRegions(file *os.File, size int64) (regions []sparseRegion, sparse bool, err error) {
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || size == 0 || int64(stat.Blocks)*512 >= size {
		return nil, false, nil
	}

	regions, err = seekDataRegions(file, size)
	if err != nil {
		return nil, false, err
	}

	return regions, true, nil
}

func seekDataRegions(file *os.File, size int64) ([]sparseRegion, error) {
	var regions []sparseRegion

	offset := int64(0)
	for offset < size {
		dataStart, err := file.Seek(offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO: the rest is a hole.
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
