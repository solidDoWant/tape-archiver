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

// blockSize is the fixed tar block size; every header and payload is padded up
// to a multiple of it.
const blockSize = 512

// writeSparseEntry emits file as a GNU sparse 1.0 (PAX GNU.sparse.*) archive
// member. The standard archive/tar writer deliberately refuses to emit
// GNU.sparse.* records, so the member is hand-rolled directly onto the
// underlying stream: a PAX extended header carrying the sparse metadata, then a
// regular-file header whose payload begins with the sparse map and holds only
// the allocated data extents. Both the shipped static GNU tar and Go's
// archive/tar reader decode this transparently, so extraction restores the holes
// as zeros without the archive ever growing to the file's logical size.
//
// The bytes are written to w.raw rather than through w.tw; w.tw is flushed first
// so the stream is at a block boundary, and every piece written here is a whole
// number of 512-byte blocks, so subsequent w.tw entries stay aligned.
func (w *tarWriter) writeSparseEntry(ctx context.Context, header *tar.Header, file *os.File, regions []sparseRegion) error {
	realSize := header.Size
	realName := header.Name

	sparseMap := buildSparseMap(regions)

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

	if err := w.tw.Flush(); err != nil {
		return fmt.Errorf("flush before sparse %s: %w", realName, err)
	}

	if err := writePaxExtendedHeader(w.raw, realName, records); err != nil {
		return fmt.Errorf("write sparse pax header %s: %w", realName, err)
	}

	dataHeader := rawHeader{
		name:     ustarName(realName),
		mode:     header.Mode,
		uid:      int64(header.Uid),
		gid:      int64(header.Gid),
		size:     physicalSize,
		modTime:  header.ModTime.Unix(),
		typeflag: tar.TypeReg,
		uname:    header.Uname,
		gname:    header.Gname,
	}
	if err := dataHeader.writeTo(w.raw); err != nil {
		return fmt.Errorf("write sparse data header %s: %w", realName, err)
	}

	if _, err := w.raw.Write(sparseMap); err != nil {
		return fmt.Errorf("write sparse map %s: %w", realName, err)
	}

	if err := w.copyRegions(ctx, file, regions); err != nil {
		return fmt.Errorf("write sparse data %s: %w", realName, err)
	}

	if pad := blockPadding(int(physicalSize)); pad > 0 {
		if _, err := w.raw.Write(make([]byte, pad)); err != nil {
			return fmt.Errorf("pad sparse data %s: %w", realName, err)
		}
	}

	return nil
}

// copyRegions writes each allocated data extent of file, in order, to the
// underlying stream, honoring ctx cancellation between extents.
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

// buildSparseMap encodes the GNU sparse 1.0 in-band map: a decimal count of data
// regions, then an offset/length pair per region, each terminated by a newline,
// padded with NUL to a 512-byte block boundary.
func buildSparseMap(regions []sparseRegion) []byte {
	var builder strings.Builder

	fmt.Fprintf(&builder, "%d\n", len(regions))

	for _, region := range regions {
		fmt.Fprintf(&builder, "%d\n%d\n", region.offset, region.length)
	}

	encoded := []byte(builder.String())
	if pad := blockPadding(len(encoded)); pad > 0 {
		encoded = append(encoded, make([]byte, pad)...)
	}

	return encoded
}

// appendTimeRecords adds PAX time records so the sparse entry preserves the same
// timestamp fidelity as the plain-file path, which the standard writer encodes as
// PAX records for sub-second times.
func appendTimeRecords(records []paxRecord, header *tar.Header) []paxRecord {
	if !header.ModTime.IsZero() {
		records = append(records, paxRecord{"mtime", formatPaxTime(header.ModTime)})
	}

	if !header.AccessTime.IsZero() {
		records = append(records, paxRecord{"atime", formatPaxTime(header.AccessTime)})
	}

	if !header.ChangeTime.IsZero() {
		records = append(records, paxRecord{"ctime", formatPaxTime(header.ChangeTime)})
	}

	return records
}

// paxRecord is one key/value pair for a PAX extended header.
type paxRecord struct {
	key   string
	value string
}

// writePaxExtendedHeader writes a PAX extended header block ('x') followed by its
// record payload padded to a block boundary. name is used only for the (cosmetic)
// header name; the records carry the meaningful data.
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

// formatPaxRecord encodes one PAX record as "<len> <key>=<value>\n", where len is
// the total byte length of the record including the length field itself.
func formatPaxRecord(key, value string) string {
	size := len(key) + len(value) + len(" =\n")
	size += len(strconv.Itoa(size))

	// The length field is self-referential: adding its own digits may push the
	// total into another decimal digit, so iterate to a fixed point.
	for size != len(strconv.Itoa(size))+len(key)+len(value)+len(" =\n") {
		size = len(strconv.Itoa(size)) + len(key) + len(value) + len(" =\n")
	}

	return fmt.Sprintf("%d %s=%s\n", size, key, value)
}

// formatPaxTime encodes a timestamp as decimal seconds with an optional
// nanosecond fraction, the PAX representation both GNU tar and Go's reader accept.
func formatPaxTime(t time.Time) string {
	if nanos := t.Nanosecond(); nanos != 0 {
		return fmt.Sprintf("%d.%09d", t.Unix(), nanos)
	}

	return strconv.FormatInt(t.Unix(), 10)
}

// rawHeader is the minimal set of fields needed to encode a POSIX ustar header
// block by hand for the sparse-file path.
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

// writeTo encodes the header as a single 512-byte ustar block and writes it.
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

// copyField copies s into field, truncating to fit and leaving the remainder as
// NUL bytes.
func copyField(field []byte, s string) {
	if len(s) > len(field) {
		s = s[:len(field)]
	}

	copy(field, s)
}

// putNumber encodes x into field as zero-padded octal with a trailing NUL, or as
// GNU base-256 when the value does not fit the octal width.
func putNumber(field []byte, x int64) {
	if x >= 0 && x < int64(1)<<uint(3*(len(field)-1)) {
		for i := range field {
			field[i] = '0'
		}

		field[len(field)-1] = 0

		octal := strconv.FormatInt(x, 8)
		copy(field[len(field)-1-len(octal):], octal)

		return
	}

	for i := len(field) - 1; i >= 0; i-- {
		field[i] = byte(x)
		x >>= 8
	}

	field[0] |= 0x80
}

// putChecksum computes and writes the header checksum: the octal sum of every
// byte with the checksum field itself taken as spaces.
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

// ustarName returns a name that fits the 100-byte ustar name field. The sparse
// entry's real name is carried in PAX records (path and GNU.sparse.name), so this
// header name is only a fallback; long names keep their tail.
func ustarName(name string) string {
	if len(name) <= 100 {
		return name
	}

	return name[len(name)-100:]
}

// paxHeaderName returns a cosmetic name for the PAX extended header block.
func paxHeaderName(name string) string {
	base := slashpath.Base(name)

	full := "PaxHeaders.0/" + base
	if len(full) > 100 {
		full = full[:100]
	}

	return full
}

// blockPadding returns the number of bytes needed to round n up to the next
// 512-byte tar block boundary.
func blockPadding(n int) int {
	if rem := n % blockSize; rem != 0 {
		return blockSize - rem
	}

	return 0
}
