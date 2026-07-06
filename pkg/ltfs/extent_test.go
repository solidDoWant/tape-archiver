package ltfs_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/ltfs"
)

// sampleIndex is a format-accurate LTFS 2.4 index covering the cases the
// extractor must handle: a byte-offset extent that starts mid-block and spans
// two blocks, a multi-extent file whose extents are listed out of FileOffset
// order, a zero-length file with no extents, and a nested directory path on the
// index ("a") partition. Block payloads for these extents live in newSampleTape.
const sampleIndex = `<?xml version="1.0" encoding="UTF-8"?>
<ltfsindex version="2.4.0">
  <creator>tape-archiver test</creator>
  <volumeuuid>00000000-0000-0000-0000-000000000000</volumeuuid>
  <generationnumber>7</generationnumber>
  <location><partition>a</partition><startblock>5</startblock></location>
  <directory>
    <name>TA0001L6</name>
    <contents>
      <file>
        <name>hello.txt</name>
        <length>11</length>
        <extentinfo>
          <extent>
            <partition>b</partition>
            <startblock>100</startblock>
            <byteoffset>3</byteoffset>
            <bytecount>11</bytecount>
            <fileoffset>0</fileoffset>
          </extent>
        </extentinfo>
      </file>
      <file>
        <name>multi.bin</name>
        <length>10</length>
        <extentinfo>
          <extent>
            <partition>b</partition>
            <startblock>200</startblock>
            <byteoffset>0</byteoffset>
            <bytecount>5</bytecount>
            <fileoffset>5</fileoffset>
          </extent>
          <extent>
            <partition>b</partition>
            <startblock>201</startblock>
            <byteoffset>2</byteoffset>
            <bytecount>5</bytecount>
            <fileoffset>0</fileoffset>
          </extent>
        </extentinfo>
      </file>
      <file>
        <name>empty</name>
        <length>0</length>
        <extentinfo></extentinfo>
      </file>
      <directory>
        <name>archives</name>
        <contents>
          <directory>
            <name>000</name>
            <contents>
              <file>
                <name>a.000</name>
                <length>4</length>
                <extentinfo>
                  <extent>
                    <partition>a</partition>
                    <startblock>10</startblock>
                    <byteoffset>0</byteoffset>
                    <bytecount>4</bytecount>
                    <fileoffset>0</fileoffset>
                  </extent>
                </extentinfo>
              </file>
            </contents>
          </directory>
        </contents>
      </directory>
    </contents>
  </directory>
</ltfsindex>`

// newSampleTape returns a fakeTape whose block payloads back sampleIndex's
// extents, laid out with a deliberately small, uneven block size so the
// extractor's "read whole blocks until the extent is covered, then slice"
// logic is exercised across block boundaries.
func newSampleTape() *fakeTape {
	return &fakeTape{blocks: map[string]map[uint64][]byte{
		"b": {
			// hello.txt: "___hello world" split 8/6; slice [3:14] == "hello world".
			100: []byte("___hello"),
			101: []byte(" world"),
			// multi.bin extent at fileoffset 5: block is exactly "world".
			200: []byte("world"),
			// multi.bin extent at fileoffset 0: "XXhello"; slice [2:7] == "hello".
			201: []byte("XXhello"),
		},
		"a": {
			// archives/000/a.000: exactly "data".
			10: []byte("data"),
		},
	}}
}

func TestParseIndex(t *testing.T) {
	t.Parallel()

	index, err := ltfs.ParseIndex([]byte(sampleIndex))
	require.NoError(t, err)

	assert.Equal(t, "TA0001L6", index.VolumeName)
	assert.Equal(t, uint64(7), index.Generation)
	assert.Equal(t, ltfs.IndexLocation{Partition: "a", StartBlock: 5}, index.Location)

	// Files() is path-sorted and carries every regular file, including the
	// nested one, at its volume-relative slash path.
	paths := make([]string, 0)
	for _, file := range index.Files() {
		paths = append(paths, file.Path)
	}

	assert.Equal(t, []string{"archives/000/a.000", "empty", "hello.txt", "multi.bin"}, paths)

	// Extents are held sorted by FileOffset regardless of index order.
	multi, ok := index.Lookup("multi.bin")
	require.True(t, ok)
	require.Len(t, multi.Extents, 2)
	assert.Equal(t, uint64(0), multi.Extents[0].FileOffset)
	assert.Equal(t, uint64(5), multi.Extents[1].FileOffset)

	nested, ok := index.Lookup("archives/000/a.000")
	require.True(t, ok)
	assert.Equal(t, "a", nested.Extents[0].Partition)

	_, ok = index.Lookup("does-not-exist")
	assert.False(t, ok)
}

func TestParseIndexErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		xml  string
	}{
		{name: "malformed XML", xml: `<ltfsindex><unclosed>`},
		{name: "wrong root element", xml: `<notanindex></notanindex>`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ltfs.ParseIndex([]byte(test.xml))
			require.Error(t, err)
		})
	}
}

func TestParseIndexEmptyVolume(t *testing.T) {
	t.Parallel()

	index, err := ltfs.ParseIndex([]byte(`<ltfsindex version="2.4.0"><generationnumber>1</generationnumber></ltfsindex>`))
	require.NoError(t, err)
	assert.Empty(t, index.Files())
}

func TestExtractFile(t *testing.T) {
	t.Parallel()

	index, err := ltfs.ParseIndex([]byte(sampleIndex))
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "byte-offset extent spanning two blocks", path: "hello.txt", want: "hello world"},
		{name: "multi-extent reassembled by file offset", path: "multi.bin", want: "helloworld"},
		{name: "nested path on the index partition", path: "archives/000/a.000", want: "data"},
		{name: "zero-length file", path: "empty", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := index.ExtractFile(t.Context(), test.path, newSampleTape())
			require.NoError(t, err)
			assert.Equal(t, test.want, string(got))
		})
	}
}

func TestExtractFileUnknown(t *testing.T) {
	t.Parallel()

	index, err := ltfs.ParseIndex([]byte(sampleIndex))
	require.NoError(t, err)

	_, err = index.ExtractFile(t.Context(), "missing.bin", newSampleTape())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestExtractFilePercentEncodedName(t *testing.T) {
	t.Parallel()

	// LTFS percent-encodes names it cannot represent directly; the parser must
	// decode them so the path matches what a recoverer looks up.
	const encoded = `<ltfsindex version="2.4.0">
  <generationnumber>1</generationnumber>
  <directory>
    <name>vol</name>
    <contents>
      <file>
        <name percentencoded="true">weird%20name.txt</name>
        <length>2</length>
        <extentinfo>
          <extent>
            <partition>b</partition><startblock>1</startblock>
            <byteoffset>0</byteoffset><bytecount>2</bytecount><fileoffset>0</fileoffset>
          </extent>
        </extentinfo>
      </file>
    </contents>
  </directory>
</ltfsindex>`

	index, err := ltfs.ParseIndex([]byte(encoded))
	require.NoError(t, err)

	_, ok := index.Lookup("weird name.txt")
	require.True(t, ok, "percent-encoded name should decode to a space")

	tape := &fakeTape{blocks: map[string]map[uint64][]byte{"b": {1: []byte("ok")}}}
	got, err := index.ExtractFile(t.Context(), "weird name.txt", tape)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(got))
}

func TestExtractFileShortRead(t *testing.T) {
	t.Parallel()

	index, err := ltfs.ParseIndex([]byte(sampleIndex))
	require.NoError(t, err)

	// A tape that hands back an empty block before the extent is covered must
	// surface an error rather than silently return truncated bytes.
	tape := &fakeTape{blocks: map[string]map[uint64][]byte{"b": {100: {}, 101: {}}, "a": {10: []byte("data")}}}
	_, err = index.ExtractFile(t.Context(), "hello.txt", tape)
	require.Error(t, err)
}

// fakeTape is an in-memory BlockReader: a partition -> block -> payload map with
// a current position that Locate sets and ReadBlock advances. It lets the
// extractor be tested to the byte with no tape hardware.
type fakeTape struct {
	blocks    map[string]map[uint64][]byte
	partition string
	block     uint64
	located   bool
}

func (f *fakeTape) Locate(_ context.Context, partition string, block uint64) error {
	f.partition = partition
	f.block = block
	f.located = true

	return nil
}

func (f *fakeTape) ReadBlock(_ context.Context) ([]byte, error) {
	if !f.located {
		return nil, fmt.Errorf("ReadBlock before Locate")
	}

	partition, ok := f.blocks[f.partition]
	if !ok {
		return nil, fmt.Errorf("no such partition %q", f.partition)
	}

	payload, ok := partition[f.block]
	if !ok {
		return nil, fmt.Errorf("no block %d in partition %q", f.block, f.partition)
	}

	f.block++

	return payload, nil
}
