package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	slashpath "path"
	"strconv"
	"strings"
	"time"
)

const blockSize = 512

// writeSparseEntry hand-rolls a GNU sparse 1.0 (PAX GNU.sparse.*) member onto
// w.raw, since archive/tar refuses to emit GNU.sparse.* records. Both GNU tar
// and Go's reader restore the holes as zeros.
//
// w.tw is flushed first and every piece here is whole 512-byte blocks, so later
// w.tw entries stay aligned.
func (w *tarWriter) writeSparseEntry(ctx context.Context, header *tar.Header, file *os.File, regions []sparseRegion) error {
	physicalSize, err := w.writeSparseHeader(header, regions)
	if err != nil {
		return err
	}

	if err := w.copyRegions(ctx, file, regions); err != nil {
		return fmt.Errorf("write sparse data %s: %w", header.Name, err)
	}

	if pad := blockPadding(int(physicalSize)); pad > 0 {
		if _, err := w.raw.Write(make([]byte, pad)); err != nil {
			return fmt.Errorf("pad sparse data %s: %w", header.Name, err)
		}
	}

	return nil
}

// writeSparseHeader writes the member's PAX header, data header, and sparse
// map, returning the member's physical size (map plus data regions).
func (w *tarWriter) writeSparseHeader(header *tar.Header, regions []sparseRegion) (int64, error) {
	realSize := header.Size
	realName := header.Name

	sparseMap := buildSparseMap(regions, realSize)

	var dataLen int64
	for _, region := range regions {
		dataLen += region.length
	}

	physicalSize := int64(len(sparseMap)) + dataLen

	records := []paxRecord{
		{"path", realName},
		{"GNU.sparse.major", "1"},
		{"GNU.sparse.minor", "0"},
		{"GNU.sparse.name", realName},
		{"GNU.sparse.realsize", strconv.FormatInt(realSize, 10)},
	}
	records = appendTimeRecords(records, header)

	// Fields that don't fit USTAR go in PAX records, as archive/tar does for
	// regular entries: GNU base-256 makes Go's reader reject the header, and
	// truncation loses data.
	dataHeader := rawHeader{
		name:     ustarName(realName),
		mode:     octalOrRecord(&records, "", 8, header.Mode),
		uid:      octalOrRecord(&records, "uid", 8, int64(header.Uid)),
		gid:      octalOrRecord(&records, "gid", 8, int64(header.Gid)),
		size:     octalOrRecord(&records, "size", 12, physicalSize),
		modTime:  octalOrRecord(&records, "", 12, header.ModTime.Unix()), // mtime record above
		typeflag: tar.TypeReg,
		uname:    stringOrRecord(&records, "uname", 32, header.Uname),
		gname:    stringOrRecord(&records, "gname", 32, header.Gname),
	}

	if err := w.tw.Flush(); err != nil {
		return 0, fmt.Errorf("flush before sparse %s: %w", realName, err)
	}

	if err := writePaxExtendedHeader(w.raw, realName, records); err != nil {
		return 0, fmt.Errorf("write sparse pax header %s: %w", realName, err)
	}

	if err := dataHeader.writeTo(w.raw); err != nil {
		return 0, fmt.Errorf("write sparse data header %s: %w", realName, err)
	}

	if _, err := w.raw.Write(sparseMap); err != nil {
		return 0, fmt.Errorf("write sparse map %s: %w", realName, err)
	}

	return physicalSize, nil
}

func (w *tarWriter) copyRegions(ctx context.Context, file *os.File, regions []sparseRegion) error {
	for _, region := range regions {
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, err := file.Seek(region.offset, io.SeekStart); err != nil {
			return err
		}

		if _, err := io.CopyN(w.raw, ctxReader{ctx: ctx, r: file}, region.length); err != nil {
			return err
		}
	}

	return nil
}

// buildSparseMap encodes the GNU sparse 1.0 in-band map (count, then
// offset/length pairs, newline-terminated, NUL-padded to a block).
//
// A file ending in a hole gets a terminal {realSize, 0} entry: GNU tar does not
// extend to GNU.sparse.realsize, so without it a trailing hole is silently
// truncated. GNU tar's own encoder emits the same entry.
func buildSparseMap(regions []sparseRegion, realSize int64) []byte {
	var builder strings.Builder

	var lastEnd int64
	for _, region := range regions {
		lastEnd = region.offset + region.length
	}

	entries := len(regions)
	if lastEnd < realSize {
		entries++
	}

	fmt.Fprintf(&builder, "%d\n", entries)

	for _, region := range regions {
		fmt.Fprintf(&builder, "%d\n%d\n", region.offset, region.length)
	}

	if lastEnd < realSize {
		fmt.Fprintf(&builder, "%d\n%d\n", realSize, 0)
	}

	encoded := []byte(builder.String())
	if pad := blockPadding(len(encoded)); pad > 0 {
		encoded = append(encoded, make([]byte, pad)...)
	}

	return encoded
}

// octalOrRecord returns x if it fits a width-byte USTAR octal field, else
// records it under key (if any) and returns 0, as archive/tar does.
func octalOrRecord(records *[]paxRecord, key string, width int, x int64) int64 {
	if fitsOctal(width, x) {
		return x
	}

	if key != "" {
		*records = append(*records, paxRecord{key, strconv.FormatInt(x, 10)})
	}

	return 0
}

// stringOrRecord returns s if it is ASCII and fits a width-byte USTAR field,
// else records it under key and returns the ASCII part, as archive/tar does.
func stringOrRecord(records *[]paxRecord, key string, width int, s string) string {
	if len(s) <= width && isASCII(s) {
		return s
	}

	*records = append(*records, paxRecord{key, s})

	return toASCII(s)
}

// appendTimeRecords records mtime to the nanosecond, matching writeEntry's PAX
// headers, which likewise carry no atime or ctime.
func appendTimeRecords(records []paxRecord, header *tar.Header) []paxRecord {
	if !header.ModTime.IsZero() {
		records = append(records, paxRecord{"mtime", formatPaxTime(header.ModTime)})
	}

	return records
}

type paxRecord struct {
	key   string
	value string
}

func writePaxExtendedHeader(w io.Writer, name string, records []paxRecord) error {
	var payload strings.Builder
	for _, record := range records {
		payload.WriteString(formatPaxRecord(record.key, record.value))
	}

	data := payload.String()

	header := rawHeader{
		name:     paxHeaderName(name),
		mode:     0o644,
		size:     int64(len(data)),
		typeflag: tar.TypeXHeader,
	}
	if err := header.writeTo(w); err != nil {
		return err
	}

	if _, err := io.WriteString(w, data); err != nil {
		return err
	}

	if pad := blockPadding(len(data)); pad > 0 {
		if _, err := w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}

	return nil
}

// formatPaxRecord encodes "len key=value\n", where len includes itself.
func formatPaxRecord(key, value string) string {
	size := len(key) + len(value) + len(" =\n")
	size += len(strconv.Itoa(size))

	// The length is self-referential; iterate to a fixed point.
	for size != len(strconv.Itoa(size))+len(key)+len(value)+len(" =\n") {
		size = len(strconv.Itoa(size)) + len(key) + len(value) + len(" =\n")
	}

	return fmt.Sprintf("%d %s=%s\n", size, key, value)
}

func formatPaxTime(t time.Time) string {
	secs, nanos := t.Unix(), t.Nanosecond()
	if nanos != 0 && secs < 0 {
		// The fraction extends the magnitude: -1.25s is Unix -2 plus 750ms.
		return fmt.Sprintf("-%d.%09d", -(secs + 1), 1_000_000_000-nanos)
	}

	if nanos != 0 {
		return fmt.Sprintf("%d.%09d", secs, nanos)
	}

	return strconv.FormatInt(t.Unix(), 10)
}

// rawHeader holds the fields for a hand-encoded ustar header block.
type rawHeader struct {
	name     string
	mode     int64
	uid      int64
	gid      int64
	size     int64
	modTime  int64
	typeflag byte
	uname    string
	gname    string
}

func (h rawHeader) writeTo(w io.Writer) error {
	var block [blockSize]byte

	copyField(block[0:100], h.name)
	putNumber(block[100:108], h.mode)
	putNumber(block[108:116], h.uid)
	putNumber(block[116:124], h.gid)
	putNumber(block[124:136], h.size)
	putNumber(block[136:148], h.modTime)
	block[156] = h.typeflag
	copy(block[257:263], "ustar\x00")
	copy(block[263:265], "00")
	copyField(block[265:297], h.uname)
	copyField(block[297:329], h.gname)

	putChecksum(&block)

	_, err := w.Write(block[:])

	return err
}

func copyField(field []byte, s string) {
	if len(s) > len(field) {
		s = s[:len(field)]
	}

	copy(field, s)
}

// putNumber encodes octal with a trailing NUL; callers ensure x fits.
func putNumber(field []byte, x int64) {
	for i := range field {
		field[i] = '0'
	}

	field[len(field)-1] = 0

	octal := strconv.FormatInt(x, 8)
	copy(field[len(field)-1-len(octal):], octal)
}

func fitsOctal(width int, x int64) bool {
	return x >= 0 && x < int64(1)<<uint(3*(width-1))
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 || s[i] == 0 {
			return false
		}
	}

	return true
}

// toASCII drops non-ASCII and NUL bytes: Go's reader rejects a header block
// holding any byte >= 0x80.
func toASCII(s string) string {
	if isASCII(s) {
		return s
	}

	var builder strings.Builder

	for i := range len(s) {
		if s[i] < 0x80 && s[i] != 0 {
			builder.WriteByte(s[i])
		}
	}

	return builder.String()
}

// putChecksum sums every byte with the checksum field taken as spaces.
func putChecksum(block *[blockSize]byte) {
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}

	sum := 0
	for _, b := range block {
		sum += int(b)
	}

	copy(block[148:154], fmt.Sprintf("%06o", sum))
	block[154] = 0
	block[155] = ' '
}

// ustarName is only a fallback; the real name is in the PAX records.
func ustarName(name string) string {
	name = toASCII(name)
	if len(name) <= 100 {
		return name
	}

	return name[len(name)-100:]
}

func paxHeaderName(name string) string {
	base := toASCII(slashpath.Base(name))

	full := "PaxHeaders.0/" + base
	if len(full) > 100 {
		full = full[:100]
	}

	return full
}

func blockPadding(n int) int {
	if rem := n % blockSize; rem != 0 {
		return blockSize - rem
	}

	return 0
}
