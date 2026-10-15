//go:build !unix

package archive

import (
	"io/fs"
	"os"
)

func hardlinkID(info fs.FileInfo) (id fileID, ok bool) {
	return fileID{}, false
}

func sparseDataRegions(file *os.File, size int64) (regions []sparseRegion, sparse bool, err error) {
	return nil, false, nil
}
