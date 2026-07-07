//go:build !unix

package archive

import (
	"io/fs"
	"os"
)

// hardlinkID cannot detect hardlinks without platform stat data, so it always
// reports ok=false and every file is stored as an independent copy.
func hardlinkID(info fs.FileInfo) (id fileID, ok bool) {
	return fileID{}, false
}

// sparseDataRegions has no portable hole-detection primitive, so it always
// reports sparse=false and files are archived by a plain contiguous copy.
func sparseDataRegions(file *os.File, size int64) (regions []sparseRegion, sparse bool, err error) {
	return nil, false, nil
}
