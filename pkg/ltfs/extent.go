package ltfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"path"
	"sort"
)

// Extent is one contiguous run of a file's bytes on tape, as recorded in the
// LTFS index. A file's content is the concatenation of its extents ordered by
// FileOffset. The fields mirror the LTFS index <extent> element (LTFS Format
// Specification 2.4): the data lives in Partition, in the ByteCount bytes that
// begin ByteOffset bytes into block StartBlock, and belong at FileOffset within
// the file.
//
// This is the byte-level map that makes index-loss recovery possible: given the
// captured index alone, an operator can SCSI-LOCATE to (Partition, StartBlock),
// read the raw blocks, slice out [ByteOffset, ByteOffset+ByteCount), and
// reassemble the exact file bytes with no working on-tape index and no LTFS
// mount (SPEC §10; issue #21 "Index-loss recovery").
type Extent struct {
	// Partition is the LTFS partition label the bytes live in: "a" (the index
	// partition) or "b" (the data partition). The mapping from label to SCSI
	// partition number is the BlockReader's concern, not this package's.
	Partition string
	// StartBlock is the first tape logical block of this extent within Partition.
	StartBlock uint64
	// ByteOffset is the offset into the first block at which this extent's bytes
	// begin (LTFS packs small files, so an extent need not be block-aligned).
	ByteOffset uint64
	// ByteCount is the number of bytes this extent contributes to the file.
	ByteCount uint64
	// FileOffset is the offset of this extent's bytes within the reconstructed
	// file. Extents are held sorted by FileOffset.
	FileOffset uint64
}

// File is one regular file in the LTFS index: its path relative to the volume
// root (slash-separated, e.g. "archives/000/archive.000"), its declared length,
// and the extents that make up its content, sorted by FileOffset.
type File struct {
	Path    string
	Length  uint64
	Extents []Extent
}

// Index is a parsed LTFS index: the volume name, the index generation, and every
// regular file keyed by path. Parse it with ParseIndex and pull a file's bytes
// back with ExtractFile.
type Index struct {
	// VolumeName is the LTFS volume name (mkltfs sets it to the tape barcode).
	VolumeName string
	// Generation is the index generation number.
	Generation uint64
	// Location is where this index generation was written on tape (its
	// <location> element) — the partition and start block of the index itself.
	// A recoverer uses it to find, or to target, the on-tape index.
	Location IndexLocation

	files map[string]*File
	order []string
}

// IndexLocation is the on-tape position of an LTFS index generation: the
// partition label ("a"/"b") and the start block within it.
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

// BlockReader positions a tape and reads its raw physical blocks. It is the
// hardware seam the extent extractor drives: ExtractFile calls Locate once per
// extent, then ReadBlock until it has the extent's bytes. pkg/tape provides the
// SCSI-backed implementation (LOCATE + READ); tests provide an in-memory fake.
// Keeping the interface primitive-typed (no ltfs types) lets pkg/tape satisfy it
// without importing this package, so there is no dependency cycle.
type BlockReader interface {
	// Locate positions the tape so the next ReadBlock returns the block at
	// (partition, block). partition is the LTFS partition label ("a" or "b").
	Locate(ctx context.Context, partition string, block uint64) error
	// ReadBlock reads the block at the current position and advances one block,
	// returning the block's payload bytes (variable length).
	ReadBlock(ctx context.Context) ([]byte, error)
}

// ParseIndex parses a captured LTFS index (the XML ReadIndex returns) into an
// Index. It walks the directory tree building slash-separated, volume-relative
// paths, and returns an error only when the document is not a well-formed LTFS
// index.
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

	// The root <directory>'s own name is the volume name; file paths are relative
	// to its contents, so recursion starts with an empty prefix.
	if err := idx.walk("", doc.Directory.Contents); err != nil {
		return nil, err
	}

	sort.Strings(idx.order)

	return idx, nil
}

// walk adds every file in contents (and its subdirectories) to the index, under
// prefix (the volume-relative path of the containing directory).
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

// ExtractFile reconstructs the named file's bytes from tape using only its
// captured extents and raw block reads through r — no LTFS mount and no working
// on-tape index required. For each extent it locates (partition, startblock),
// reads whole blocks until it has ByteOffset+ByteCount bytes, slices out the
// extent's data, and places it at FileOffset in the result. It returns an error
// if the file is unknown or a read comes up short.
func (idx *Index) ExtractFile(ctx context.Context, name string, r BlockReader) ([]byte, error) {
	file, ok := idx.files[name]
	if !ok {
		return nil, fmt.Errorf("file %q not found in LTFS index", name)
	}

	out := make([]byte, file.Length)

	for _, extent := range file.Extents {
		data, err := readExtent(ctx, r, extent)
		if err != nil {
			return nil, fmt.Errorf("read extent of %q at partition %s block %d: %w", name, extent.Partition, extent.StartBlock, err)
		}

		end := extent.FileOffset + uint64(len(data))
		if end > file.Length {
			return nil, fmt.Errorf("extent of %q at file offset %d overruns declared length %d", name, extent.FileOffset, file.Length)
		}

		copy(out[extent.FileOffset:], data)
	}

	return out, nil
}

// readExtent positions r at the extent's start block and reads whole blocks until
// it has at least ByteOffset+ByteCount bytes, then returns exactly the extent's
// [ByteOffset, ByteOffset+ByteCount) slice.
func readExtent(ctx context.Context, r BlockReader, extent Extent) ([]byte, error) {
	if err := r.Locate(ctx, extent.Partition, extent.StartBlock); err != nil {
		return nil, err
	}

	need := extent.ByteOffset + extent.ByteCount

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

// xmlIndex mirrors the parts of the LTFS index document this package reads: the
// generation number and the root directory tree. Elements it does not need
// (creator, volumeuuid, timestamps, policies) are ignored by encoding/xml.
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

// xmlName is an LTFS index <name> element: its text plus the percentencoded
// attribute LTFS sets when the name cannot be represented directly in the XML.
type xmlName struct {
	Value          string `xml:",chardata"`
	PercentEncoded bool   `xml:"percentencoded,attr"`
}

// decode returns the name, percent-decoding it when the element carried
// percentencoded="true". A value that fails to decode is returned unchanged
// rather than dropped.
func (n xmlName) decode() string {
	if !n.PercentEncoded {
		return n.Value
	}

	if decoded, err := url.QueryUnescape(n.Value); err == nil {
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
