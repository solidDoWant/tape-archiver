//go:build unix

package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// TestSparseHeaderFieldsUsePAX checks sparse members carry every field that
// does not fit USTAR as a PAX record, so Go's reader and GNU tar (the recovery
// disc's extractor) both read the exact values back.
func TestSparseHeaderFieldsUsePAX(t *testing.T) {
	t.Parallel()

	const (
		maxOctal8  = 1<<21 - 1 // largest value an 8-byte USTAR octal field holds
		eightGiB   = int64(8) << 30
		longName32 = "abcdefghijklmnopqrstuvwxyz012345" // exactly 32 bytes
	)

	base := func() *tar.Header {
		return &tar.Header{
			Name:    "disk.img",
			Mode:    0o640,
			Uid:     1000,
			Gid:     1000,
			Uname:   "user",
			Gname:   "group",
			Size:    1 << 20,
			ModTime: time.Unix(1_739_941_425, 615_599_752),
		}
	}

	small := []archive.SparseRegion{{Offset: 0, Length: 4096}}

	testCases := []struct {
		name    string
		modify  func(header *tar.Header)
		regions []archive.SparseRegion
		// slow cases stream > 8 GiB through GNU tar.
		slow bool
	}{
		{name: "fits ustar", modify: func(*tar.Header) {}},
		{
			name: "uid and gid at the octal limit",
			modify: func(header *tar.Header) {
				header.Uid, header.Gid = maxOctal8, maxOctal8
			},
		},
		{
			name: "uid and gid one above the octal limit",
			modify: func(header *tar.Header) {
				header.Uid, header.Gid = maxOctal8+1, maxOctal8+1
			},
		},
		{
			name: "uid and gid above the octal limit",
			modify: func(header *tar.Header) {
				header.Uid, header.Gid = 3_000_000, 4_000_000
			},
		},
		{
			name: "uname and gname of exactly 32 bytes",
			modify: func(header *tar.Header) {
				header.Uname, header.Gname = longName32, longName32
			},
		},
		{
			name: "uname and gname longer than 32 bytes",
			modify: func(header *tar.Header) {
				header.Uname = strings.Repeat("u", 33)
				header.Gname = strings.Repeat("g", 64)
			},
		},
		{
			name: "non-ascii uname, gname, and name",
			modify: func(header *tar.Header) {
				header.Name = "héllo_日本語.img"
				header.Uname, header.Gname = "jürgen", "grüppe"
			},
		},
		{
			name: "name longer than 100 bytes",
			modify: func(header *tar.Header) {
				header.Name = strings.Repeat("d", 90) + "/" + strings.Repeat("f", 120) + ".img"
			},
		},
		{
			name: "mtime before 1970",
			modify: func(header *tar.Header) {
				header.ModTime = time.Unix(-86_400, 250)
			},
		},
		{
			name: "logical size above 8 GiB",
			modify: func(header *tar.Header) {
				header.Size = 9 * eightGiB / 8
			},
			regions: []archive.SparseRegion{{Offset: 0, Length: 4096}, {Offset: 9*eightGiB/8 - 4096, Length: 4096}},
		},
		{
			name: "stored data above 8 GiB",
			modify: func(header *tar.Header) {
				header.Size = 2 * eightGiB
			},
			regions: []archive.SparseRegion{{Offset: 0, Length: eightGiB + 4096}},
			slow:    true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			header := base()
			testCase.modify(header)

			regions := testCase.regions
			if regions == nil {
				regions = small
			}

			var preamble bytes.Buffer

			require.NoError(t, archive.WriteSparseHeader(&preamble, header, regions))

			// Go's reader needs only the preamble: it parses the in-band map
			// and returns the header without reading the data.
			read, err := tar.NewReader(bytes.NewReader(preamble.Bytes())).Next()
			require.NoError(t, err)

			assert.Equal(t, tar.FormatPAX, read.Format, "the header must parse as PAX, not unknown")
			assert.Equal(t, header.Name, read.Name)
			assert.Equal(t, header.Mode, read.Mode)
			assert.Equal(t, header.Uid, read.Uid)
			assert.Equal(t, header.Gid, read.Gid)
			assert.Equal(t, header.Uname, read.Uname)
			assert.Equal(t, header.Gname, read.Gname)
			assert.Equal(t, header.Size, read.Size)
			assert.True(t, header.ModTime.Equal(read.ModTime), "mtime %v, want %v", read.ModTime, header.ModTime)

			if testCase.slow && testing.Short() {
				t.Log("skipping the GNU tar listing: -short and this case streams over 8 GiB")

				return
			}

			listing := listWithGNUTar(t, completeSparseMember(preamble.Bytes(), regions), "--numeric-owner")
			assert.Contains(t, listing, strconv.Itoa(header.Uid)+"/"+strconv.Itoa(header.Gid)+" ",
				"GNU tar must read the exact uid/gid")
			assert.Contains(t, listing, " "+strconv.FormatInt(header.Size, 10)+" ",
				"GNU tar must read the logical size")
			assert.Contains(t, listing, header.Name+"\n")
			assert.Contains(t, listing, "trailer\n", "the member must end where its header says")

			named := listWithGNUTar(t, completeSparseMember(preamble.Bytes(), regions))
			assert.Contains(t, named, header.Uname+"/"+header.Gname+" ",
				"GNU tar must read the full uname/gname")
		})
	}
}

// completeSparseMember follows a sparse preamble with zeroed data, then a
// plain "trailer" member and the end-of-archive blocks.
func completeSparseMember(preamble []byte, regions []archive.SparseRegion) io.Reader {
	var dataLen int64
	for _, region := range regions {
		dataLen += region.Length
	}

	// The preamble ends with the map, which is whole blocks, so the data
	// padding depends on the data length alone.
	padding := (512 - dataLen%512) % 512

	var trailer bytes.Buffer

	writer := tar.NewWriter(&trailer)
	_ = writer.WriteHeader(&tar.Header{Name: "trailer", Mode: 0o644, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
	_ = writer.Close()

	return io.MultiReader(
		bytes.NewReader(preamble),
		io.LimitReader(zeroReader{}, dataLen+padding),
		&trailer,
	)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

func listWithGNUTar(t *testing.T, archived io.Reader, extraArgs ...string) string {
	t.Helper()

	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar binary available to validate GNU tar listing")
	}

	args := append([]string{"-t", "-v", "-f", "-"}, extraArgs...)
	cmd := exec.CommandContext(t.Context(), "tar", args...)
	cmd.Stdin = archived

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	require.NoError(t, cmd.Run(), "gnu tar listing failed: %s", stderr.String())
	require.Empty(t, stderr.String(), "gnu tar must not warn")

	return stdout.String()
}

// TestSparseEntryRestoresLargeOwner archives a real sparse file under an owner
// whose uid/gid and names do not fit USTAR, then extracts it with GNU tar and
// Go's reader.
func TestSparseEntryRestoresLargeOwner(t *testing.T) {
	t.Parallel()

	const logical = int64(4 << 20)

	src := t.TempDir()
	path := filepath.Join(src, "disk.img")
	want := writeSparseFixture(t, path, logical, []dataExtent{
		{offset: 0, data: bytes.Repeat([]byte("H"), 8<<10)},
		{offset: logical - 8<<10, data: bytes.Repeat([]byte("T"), 8<<10)},
	})

	info, err := os.Stat(path)
	require.NoError(t, err)

	header, err := tar.FileInfoHeader(info, "")
	require.NoError(t, err)

	header.Name = "disk.img"
	header.Uid, header.Gid = 3_000_000, 4_000_000
	// Names no system account has, so GNU tar falls back to the ids.
	header.Uname = "tape-archiver-test-" + strings.Repeat("u", 30)
	header.Gname = "tape-archiver-test-" + strings.Repeat("g", 30)

	file, err := os.Open(path)
	require.NoError(t, err)

	defer func() { _ = file.Close() }()

	var buf bytes.Buffer

	require.NoError(t, archive.WriteSparseFile(t.Context(), &buf, header, file))

	read, err := tar.NewReader(bytes.NewReader(buf.Bytes())).Next()
	require.NoError(t, err)
	assert.Equal(t, tar.FormatPAX, read.Format)
	assert.Equal(t, header.Uid, read.Uid)
	assert.Equal(t, header.Gid, read.Gid)
	assert.Equal(t, header.Uname, read.Uname)
	assert.Equal(t, header.Gname, read.Gname)

	goDest := t.TempDir()
	extractTar(t, bytes.NewReader(buf.Bytes()), goDest)

	got, err := os.ReadFile(filepath.Join(goDest, "disk.img"))
	require.NoError(t, err)
	assert.Equal(t, want, got, "Go's reader must restore the contents with holes intact")

	gnuDest := t.TempDir()
	extractWithGNUTar(t, buf.Bytes(), gnuDest)

	extracted := filepath.Join(gnuDest, "disk.img")

	got, err = os.ReadFile(extracted)
	require.NoError(t, err)
	assert.Equal(t, want, got, "GNU tar must restore the contents with holes intact")

	if os.Geteuid() != 0 {
		t.Log("skipping the restored-owner check: GNU tar only restores ownership as root")

		return
	}

	stat, err := os.Stat(extracted)
	require.NoError(t, err)

	owner := stat.Sys().(*syscall.Stat_t)
	assert.Equal(t, uint32(header.Uid), owner.Uid, "GNU tar must restore the exact uid")
	assert.Equal(t, uint32(header.Gid), owner.Gid, "GNU tar must restore the exact gid")
}
