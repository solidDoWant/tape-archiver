//go:build integration

package optical_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// TestBurnVerifyBlankReburn burns, verifies, blanks, and reburns a loop-device
// pseudo-disc. Write-once states need real media; see TestBlankRefusesWriteOnceMedium.
func TestBurnVerifyBlankReburn(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)
	testutil.SkipIfLosetupUnavailable(t)

	device := loopDevice(t, 64<<20) // 64 MiB backing, ample for a tens-of-MB ISO
	disc := optical.NewDisc(device)

	state, err := disc.State(t.Context())
	require.NoError(t, err, "state of fresh disc")
	assert.Equal(t, optical.StateBlank, state, "a fresh loop-backed disc should be blank")

	source := map[string]string{
		"report.pdf":           "pretend PDF report bytes\n",
		"recovery.txt":         "step-by-step recovery procedure\n",
		"bin/age":              "pretend static age binary\n",
		"ltfs-index/T1.schema": "<index>tape one</index>\n",
	}
	isoPath := makeISO(t, source)

	require.NoError(t, disc.WriteImage(t.Context(), isoPath), "burn recovery ISO")

	state, err = disc.State(t.Context())
	require.NoError(t, err, "state after burn")
	assert.Equal(t, optical.StateNonBlankRewritable, state,
		"a written overwriteable disc should be non-blank rewritable")

	manifest := manifestFor(source)

	result, err := disc.Verify(t.Context(), manifest)
	require.NoError(t, err, "verify burned disc")
	assert.True(t, result.OK(), "burned disc should match its manifest: %v", result.Err())

	t.Run("mismatch is a verification failure", func(t *testing.T) {
		bad := cloneManifest(manifest)
		bad["report.pdf"] = sha256Hex("tampered")

		result, err := disc.Verify(t.Context(), bad)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"report.pdf"}, result.Mismatched)
	})

	t.Run("missing file is a verification failure", func(t *testing.T) {
		bad := cloneManifest(manifest)
		bad["not-on-disc.txt"] = sha256Hex("absent")

		result, err := disc.Verify(t.Context(), bad)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"not-on-disc.txt"}, result.Missing)
	})

	t.Run("extra file is a verification failure", func(t *testing.T) {
		bad := cloneManifest(manifest)
		delete(bad, "bin/age")

		result, err := disc.Verify(t.Context(), bad)
		require.NoError(t, err)
		assert.False(t, result.OK())
		assert.Equal(t, []string{"bin/age"}, result.Extra)
	})

	require.NoError(t, disc.Blank(t.Context()), "blank rewritable disc")

	state, err = disc.State(t.Context())
	require.NoError(t, err, "state after blank")
	assert.Equal(t, optical.StateBlank, state, "blanked disc should report blank again")

	require.NoError(t, disc.WriteImage(t.Context(), isoPath), "re-burn after blank")

	result, err = disc.Verify(t.Context(), manifest)
	require.NoError(t, err, "verify re-burned disc")
	assert.True(t, result.OK(), "re-burned disc should match its manifest: %v", result.Err())
}

// TestBlankRefusesWriteOnceMedium needs real write-once media via OPTICAL_BURN_DEV.
func TestBlankRefusesWriteOnceMedium(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)

	disc := optical.NewDisc(testutil.OpticalBurnDev(t))

	state, err := disc.State(t.Context())
	require.NoError(t, err, "state of write-once medium")
	require.NotEqual(t, optical.StateNonBlankRewritable, state,
		"%s must point at write-once media for this assertion", testutil.OpticalBurnDevEnv)

	err = disc.Blank(t.Context())
	require.Error(t, err, "Blank must refuse a write-once medium")
	assert.Contains(t, err.Error(), "write-once")
}

// loopDevice attaches a size-byte backing file to a loop device. Requires root.
func loopDevice(t *testing.T, size int64) string {
	t.Helper()

	backing := filepath.Join(t.TempDir(), "disc.img")
	require.NoError(t, os.WriteFile(backing, nil, 0o600), "create backing file")
	require.NoError(t, os.Truncate(backing, size), "size backing file")

	out, err := exec.CommandContext(t.Context(), "losetup", "--find", "--show", backing).CombinedOutput()
	require.NoError(t, err, "attach loop device: %s", strings.TrimSpace(string(out)))

	device := strings.TrimSpace(string(out))
	require.NotEmpty(t, device, "losetup returned no device")

	t.Cleanup(func() {
		// Survive the test's own cancellation so the loop device is always freed.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = exec.CommandContext(ctx, "losetup", "--detach", device).Run()
	})

	return device
}

// makeISO builds an ISO 9660 image from a path -> content map.
func makeISO(t *testing.T, files map[string]string) string {
	t.Helper()

	stage := t.TempDir()
	for name, content := range files {
		path := filepath.Join(stage, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	isoPath := filepath.Join(t.TempDir(), "recovery.iso")

	out, err := exec.CommandContext(t.Context(), "xorriso",
		"-as", "mkisofs", "-o", isoPath, "-V", "OPTICAL_IT", stage).CombinedOutput()
	require.NoError(t, err, "build ISO: %s", strings.TrimSpace(string(out)))

	return isoPath
}

func manifestFor(files map[string]string) optical.Manifest {
	manifest := make(optical.Manifest, len(files))
	for name, content := range files {
		manifest[name] = sha256Hex(content)
	}

	return manifest
}

func cloneManifest(manifest optical.Manifest) optical.Manifest {
	clone := make(optical.Manifest, len(manifest))
	for name, digest := range manifest {
		clone[name] = digest
	}

	return clone
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:])
}
