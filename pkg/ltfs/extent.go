package ltfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"path"
	"sort"
)

// Extent is one contiguous run of a file's bytes on tape, mirroring the LTFS
// index <extent> element (LTFS Format Specification 2.4). It is the byte-level
// map that enables index-loss recovery from raw blocks with no LTFS mount
// (SPEC §10; #21).
type Extent struct {
	// Partition is the LTFS partition label: "a" (index) or "b" (data).
	Partition  string
	StartBlock uint64
	// ByteOffset is where the bytes begin in StartBlock; LTFS packs small files,
	// so extents need not be block-aligned.
	ByteOffset uint64
	ByteCount  uint64
	FileOffset uint64
}

// File is one regular file in the LTFS index, with its slash-separated
// volume-relative path and its extents sorted by FileOffset.
type File struct {
	Path    string
	Length  uint64
	Extents []Extent
}

// Index is a parsed LTFS index (see ParseIndex).
type Index struct {
	// VolumeName is the LTFS volume name (mkltfs sets it to the tape barcode).
	VolumeName string
	Generation uint64
	// Location is where this index generation itself was written on tape.
	Location IndexLocation

	files map[string]*File
	order []string
}

// IndexLocation is the on-tape position of an LTFS index generation.
type IndexLocation struct {
	Partition  string
	StartBlock uint64
}

// Files returns every file in the index, in a deterministic (path-sorted) order.
func (idx *Index) Files() []File {
	out := make([]File, 0, len(idx.order))
	for _, name := range idx.order {
		out = append(out, *idx.files[name])
	}

	return out
}

// Lookup returns the file at the given volume-relative, slash-separated path.
func (idx *Index) Lookup(name string) (File, bool) {
	f, ok := idx.files[name]
	if !ok {
		return File{}, false
	}

	return *f, true
}

// BlockReader positions a tape and reads its raw blocks; pkg/tape implements it
// over SCSI. It is primitive-typed so pkg/tape need not import this package.
type BlockReader interface {
	// Locate positions the tape at (partition label, block).
	Locate(ctx context.Context, partition string, block uint64) error
	// ReadBlock reads the current block and advances one block.
	ReadBlock(ctx context.Context) ([]byte, error)
}

// ParseIndex parses a captured LTFS index (the XML ReadIndex returns).
func ParseIndex(data []byte) (*Index, error) {
	var doc xmlIndex
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse LTFS index XML: %w", err)
	}

	if doc.XMLName.Local != "ltfsindex" {
		return nil, fmt.Errorf("unexpected root element %q, want ltfsindex", doc.XMLName.Local)
	}

	idx := &Index{
		VolumeName: doc.Directory.Name.decode(),
		Generation: doc.Generation,
		Location:   IndexLocation{Partition: doc.Location.Partition, StartBlock: doc.Location.StartBlock},
		files:      make(map[string]*File),
	}

	// The root <directory>'s name is the volume name, not a path component.
	if err := idx.walk("", doc.Directory.Contents); err != nil {
		return nil, err
	}

	sort.Strings(idx.order)

	return idx, nil
}

// walk adds every file in contents, recursively, under prefix.
func (idx *Index) walk(prefix string, contents xmlContents) error {
	for _, file := range contents.Files {
		name := file.Name.decode()
		if name == "" {
			return fmt.Errorf("LTFS index file under %q has an empty name", prefix)
		}

		full := name
		if prefix != "" {
			full = path.Join(prefix, name)
		}

		if _, dup := idx.files[full]; dup {
			return fmt.Errorf("LTFS index lists path %q more than once", full)
		}

		extents := make([]Extent, 0, len(file.Extents))
		for _, extent := range file.Extents {
			extents = append(extents, Extent(extent))
		}

		sort.Slice(extents, func(i, j int) bool {
			return extents[i].FileOffset < extents[j].FileOffset
		})

		idx.files[full] = &File{Path: full, Length: file.Length, Extents: extents}
		idx.order = append(idx.order, full)
	}

	for _, dir := range contents.Directories {
		name := dir.Name.decode()
		if name == "" {
			return fmt.Errorf("LTFS index directory under %q has an empty name", prefix)
		}

		child := name
		if prefix != "" {
			child = path.Join(prefix, name)
		}

		if err := idx.walk(child, dir.Contents); err != nil {
			return err
		}
	}

	return nil
}

// ExtractFile reconstructs the named file's bytes using only its captured
// extents and raw block reads through r, with no LTFS mount or on-tape index.
func (idx *Index) ExtractFile(ctx context.Context, name string, r BlockReader) ([]byte, error) {
	file, ok := idx.files[name]
	if !ok {
		return nil, fmt.Errorf("file %q not found in LTFS index", name)
	}

	// A corrupt index length would make make() panic.
	if file.Length > math.MaxInt {
		return nil, fmt.Errorf("file %q declares length %d exceeding the addressable maximum %d", name, file.Length, math.MaxInt)
	}

	out := make([]byte, file.Length)

	for _, extent := range file.Extents {
		data, err := readExtent(ctx, r, extent, file.Length)
		if err != nil {
			return nil, fmt.Errorf("read extent of %q at partition %s block %d: %w", name, extent.Partition, extent.StartBlock, err)
		}

		copy(out[extent.FileOffset:], data)
	}

	return out, nil
}

// readExtent reads the extent's bytes through r. Corrupt ranges, including any
// that cannot fit declaredLength, are rejected before any tape work so neither
// this nor the caller's copy can panic.
func readExtent(ctx context.Context, r BlockReader, extent Extent, declaredLength uint64) ([]byte, error) {
	// A uint64 wrap would end the read loop early and panic the final slice.
	if extent.ByteCount > math.MaxUint64-extent.ByteOffset {
		return nil, fmt.Errorf("extent byte range overflows: byteoffset %d + bytecount %d", extent.ByteOffset, extent.ByteCount)
	}

	// Beyond math.MaxInt make() panics; the declaredLength check below cannot
	// catch a huge ByteOffset.
	need := extent.ByteOffset + extent.ByteCount
	if need > math.MaxInt {
		return nil, fmt.Errorf("extent byte range %d (byteoffset %d + bytecount %d) exceeds the addressable maximum %d", need, extent.ByteOffset, extent.ByteCount, math.MaxInt)
	}

	// Compared term-wise because FileOffset+ByteCount can itself wrap.
	if extent.FileOffset > declaredLength || extent.ByteCount > declaredLength-extent.FileOffset {
		return nil, fmt.Errorf("extent at fileoffset %d bytecount %d overruns declared length %d", extent.FileOffset, extent.ByteCount, declaredLength)
	}

	if err := r.Locate(ctx, extent.Partition, extent.StartBlock); err != nil {
		return nil, err
	}

	buf := make([]byte, 0, need)
	for uint64(len(buf)) < need {
		block, err := r.ReadBlock(ctx)
		if err != nil {
			return nil, err
		}

		if len(block) == 0 {
			return nil, fmt.Errorf("read returned an empty block before %d bytes were available", need)
		}

		buf = append(buf, block...)
	}

	return buf[extent.ByteOffset : extent.ByteOffset+extent.ByteCount], nil
}

// xmlIndex mirrors the parts of the LTFS index document this package reads.
type xmlIndex struct {
	XMLName    xml.Name    `xml:"ltfsindex"`
	Generation uint64      `xml:"generationnumber"`
	Location   xmlLocation `xml:"location"`
	Directory  xmlDir      `xml:"directory"`
}

type xmlLocation struct {
	Partition  string `xml:"partition"`
	StartBlock uint64 `xml:"startblock"`
}

type xmlDir struct {
	Name     xmlName     `xml:"name"`
	Contents xmlContents `xml:"contents"`
}

type xmlContents struct {
	Files       []xmlFile `xml:"file"`
	Directories []xmlDir  `xml:"directory"`
}

type xmlFile struct {
	Name    xmlName     `xml:"name"`
	Length  uint64      `xml:"length"`
	Extents []xmlExtent `xml:"extentinfo>extent"`
}

// xmlName is an LTFS <name> element, percent-encoded when LTFS cannot represent
// it directly in XML.
type xmlName struct {
	Value          string `xml:",chardata"`
	PercentEncoded bool   `xml:"percentencoded,attr"`
}

// decode returns the name, percent-decoded if flagged; undecodable values are
// returned unchanged.
func (n xmlName) decode() string {
	if !n.PercentEncoded {
		return n.Value
	}

	// PathUnescape, not QueryUnescape: LTFS names are %XX path-escaped, so a
	// literal "+" must stay "+" (QueryUnescape would turn it into a space).
	if decoded, err := url.PathUnescape(n.Value); err == nil {
		return decoded
	}

	return n.Value
}

type xmlExtent struct {
	Partition  string `xml:"partition"`
	StartBlock uint64 `xml:"startblock"`
	ByteOffset uint64 `xml:"byteoffset"`
	ByteCount  uint64 `xml:"bytecount"`
	FileOffset uint64 `xml:"fileoffset"`
}
