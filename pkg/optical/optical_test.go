package optical

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOpticalDeviceAndDriveAddress(t *testing.T) {
	tests := []struct {
		name        string
		device      string
		wantOptical bool
		wantAddress string
	}{
		{name: "real sr drive", device: "/dev/sr0", wantOptical: true, wantAddress: "/dev/sr0"},
		{name: "real scd drive", device: "/dev/scd1", wantOptical: true, wantAddress: "/dev/scd1"},
		{name: "cdrom symlink", device: "/dev/cdrom", wantOptical: true, wantAddress: "/dev/cdrom"},
		{name: "loop device wrapped in stdio", device: "/dev/loop3", wantOptical: false, wantAddress: "stdio:/dev/loop3"},
		{name: "regular file wrapped in stdio", device: "/tmp/disc.img", wantOptical: false, wantAddress: "stdio:/tmp/disc.img"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.wantOptical, isOpticalDevice(test.device))
			assert.Equal(t, test.wantAddress, NewDisc(test.device).driveAddress())
		})
	}
}

func TestDiscStateString(t *testing.T) {
	assert.Equal(t, "blank", StateBlank.String())
	assert.Equal(t, "appendable-write-once", StateAppendableWriteOnce.String())
	assert.Equal(t, "non-blank-rewritable", StateNonBlankRewritable.String())
	assert.Equal(t, "finalized", StateFinalized.String())
	assert.Equal(t, "unknown", StateUnknown.String())
}

func TestParseMediaReport(t *testing.T) {
	tests := []struct {
		name           string
		report         string
		wantState      DiscState
		wantRewritable bool
		assertErr      require.ErrorAssertionFunc
	}{
		{
			// Captured from the real xorriso 1.5.6 against a fresh stdio file.
			name: "blank stdio pseudo-disc",
			report: "Drive current: -indev 'stdio:blank.img'\n" +
				"Media current: stdio file, overwriteable\n" +
				"Media status : is blank\n" +
				"Media summary: 0 sessions, 0 data blocks, 0 data, 10.0g free\n",
			wantState:      StateBlank,
			wantRewritable: true,
		},
		{
			// Captured after burning an image to the stdio pseudo-disc: an
			// overwriteable medium reports "is appendable" yet is rewritable.
			name: "written overwriteable medium",
			report: "Media current: stdio file, overwriteable\n" +
				"Media status : is written , is appendable\n",
			wantState:      StateNonBlankRewritable,
			wantRewritable: true,
		},
		{
			name: "non-blank rewritable DVD+RW",
			report: "Media current: DVD+RW\n" +
				"Media status : is written\n",
			wantState:      StateNonBlankRewritable,
			wantRewritable: true,
		},
		{
			name: "appendable write-once DVD-R (M-DISC)",
			report: "Media current: DVD-R\n" +
				"Media status : is written , is appendable\n",
			wantState:      StateAppendableWriteOnce,
			wantRewritable: false,
		},
		{
			name: "finalized write-once medium",
			report: "Media current: DVD-R\n" +
				"Media status : is written , is closed\n",
			wantState:      StateFinalized,
			wantRewritable: false,
		},
		{
			name: "blank write-once medium",
			report: "Media current: DVD-R\n" +
				"Media status : is blank\n",
			wantState:      StateBlank,
			wantRewritable: false,
		},
		{
			name:      "no medium loaded",
			report:    "Drive current: -indev '/dev/sr0'\n",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			info, err := parseMediaReport(test.report)
			test.assertErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.wantState, info.state)
			assert.Equal(t, test.wantRewritable, info.rewritable)
		})
	}
}

func TestParseManifest(t *testing.T) {
	const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	tests := []struct {
		name      string
		input     string
		want      Manifest
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:  "text and binary separators, leading ./ and blank lines",
			input: digestA + "  report.pdf\n\n" + digestB + " *bin/age\n" + digestA + "  ./ltfs-index/T1.schema\n",
			want: Manifest{
				"report.pdf":           digestA,
				"bin/age":              digestB,
				"ltfs-index/T1.schema": digestA,
			},
		},
		{
			name:  "uppercase digest is normalized to lowercase",
			input: strings.ToUpper(digestA) + "  report.pdf\n",
			want:  Manifest{"report.pdf": digestA},
		},
		{
			name:      "missing path",
			input:     digestA + "\n",
			assertErr: require.Error,
		},
		{
			name:      "short digest",
			input:     "abc  report.pdf\n",
			assertErr: require.Error,
		},
		{
			name:      "duplicate path",
			input:     digestA + "  report.pdf\n" + digestB + "  report.pdf\n",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			manifest, err := ParseManifest(strings.NewReader(test.input))
			test.assertErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.want, manifest)
		})
	}
}

// TestVerifyTree exercises the pure disc-vs-manifest comparison against a plain
// temp directory standing in for a mounted disc — no xorriso, mount, or burner
// needed. This is the highest-value, most bug-prone logic (AC2), so it is unit
// tested directly for match and every failure mode.
func TestVerifyTree(t *testing.T) {
	root := t.TempDir()

	files := map[string]string{
		"report.pdf":           "pdf bytes",
		"manifest.sha256":      "manifest bytes",
		"bin/age":              "age binary",
		"ltfs-index/T1.schema": "index xml",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	digest := func(content string) string {
		sum := sha256.Sum256([]byte(content))

		return hex.EncodeToString(sum[:])
	}

	fullManifest := func() Manifest {
		manifest := make(Manifest, len(files))
		for name, content := range files {
			manifest[name] = digest(content)
		}

		return manifest
	}

	t.Run("matching disc verifies", func(t *testing.T) {
		result, err := verifyTree(root, fullManifest())
		require.NoError(t, err)
		assert.True(t, result.OK())
		assert.NoError(t, result.Err())
	})

	t.Run("mismatched file is reported", func(t *testing.T) {
		manifest := fullManifest()
		manifest["report.pdf"] = digest("different bytes")

		result, err := verifyTree(root, manifest)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"report.pdf"}, result.Mismatched)
		assert.ErrorContains(t, result.Err(), "mismatched: report.pdf")
	})

	t.Run("missing file is reported", func(t *testing.T) {
		manifest := fullManifest()
		manifest["absent.txt"] = digest("never burned")

		result, err := verifyTree(root, manifest)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"absent.txt"}, result.Missing)
		assert.ErrorContains(t, result.Err(), "missing: absent.txt")
	})

	t.Run("extra file is reported", func(t *testing.T) {
		manifest := fullManifest()
		delete(manifest, "bin/age")

		result, err := verifyTree(root, manifest)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"bin/age"}, result.Extra)
		assert.ErrorContains(t, result.Err(), "extra: bin/age")
	})
}
