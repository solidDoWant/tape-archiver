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

// TestBurnVerifyBlankReburn exercises the full physical optical path against a
// loop-device-backed pseudo-disc: burn a recovery ISO, read it back by mounting
// read-only and verifying every file against a manifest (including the
// mismatch/missing/extra failure modes), observe the medium state through each
// transition, then reclaim the rewritable disc with Blank and burn again. It
// backs the "disc" with a real losetup'd block device so the mount read-back path
// runs exactly as it would on hardware; it covers ACs 1–4 for the states a
// file-backed medium can represent. (True write-once states — appendable/
// finalized, and Blank's refusal of them — need real media and are covered by
// TestBlankRefusesWriteOnceMedium, gated behind OPTICAL_BURN_DEV.)
func TestBurnVerifyBlankReburn(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)
	testutil.SkipIfLosetupUnavailable(t)

	device := loopDevice(t, 64<<20) // 64 MiB backing, ample for a tens-of-MB ISO
	disc := optical.NewDisc(device)

	// --- Blank to start (AC3: a fresh medium reports blank) --------------------
	state, err := disc.State(t.Context())
	require.NoError(t, err, "state of fresh disc")
	assert.Equal(t, optical.StateBlank, state, "a fresh loop-backed disc should be blank")

	// --- Burn (AC1: a prepared ISO burns to a mountable ISO 9660 filesystem) ---
	source := map[string]string{
		"report.pdf":           "pretend PDF report bytes\n",
		"recovery.txt":         "step-by-step recovery procedure\n",
		"bin/age":              "pretend static age binary\n",
		"ltfs-index/T1.schema": "<index>tape one</index>\n",
	}
	isoPath := makeISO(t, source)

	require.NoError(t, disc.WriteImage(t.Context(), isoPath), "burn recovery ISO")

	// --- State after burn (AC3: an overwriteable medium is non-blank) ----------
	state, err = disc.State(t.Context())
	require.NoError(t, err, "state after burn")
	assert.Equal(t, optical.StateNonBlankRewritable, state,
		"a written overwriteable disc should be non-blank rewritable")

	// --- Read-back verify: matching disc (ACs 1 & 2) ---------------------------
	manifest := manifestFor(source)

	result, err := disc.Verify(t.Context(), manifest)
	require.NoError(t, err, "verify burned disc")
	assert.True(t, result.OK(), "burned disc should match its manifest: %v", result.Err())

	// --- Read-back verify: failure modes (AC2) ---------------------------------
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

	// --- Blank reclaims the rewritable disc (AC4) ------------------------------
	require.NoError(t, disc.Blank(t.Context()), "blank rewritable disc")

	state, err = disc.State(t.Context())
	require.NoError(t, err, "state after blank")
	assert.Equal(t, optical.StateBlank, state, "blanked disc should report blank again")

	// --- A subsequent burn succeeds after the reclaim (AC4) --------------------
	require.NoError(t, disc.WriteImage(t.Context(), isoPath), "re-burn after blank")

	result, err = disc.Verify(t.Context(), manifest)
	require.NoError(t, err, "verify re-burned disc")
	assert.True(t, result.OK(), "re-burned disc should match its manifest: %v", result.Err())
}

// TestBlankRefusesWriteOnceMedium asserts that Blank refuses a write-once medium
// rather than silently succeeding (AC4). A file-backed stdio disc is always
// overwriteable, so this transition needs real write-once media; it is skipped
// unless OPTICAL_BURN_DEV points at one.
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

// loopDevice creates a size-byte backing file and attaches it to a loop device,
// returning the loop device path (e.g. /dev/loop0). Both the loop device and the
// backing file are cleaned up when the test ends. Requires root (losetup), which
// the integration target provides.
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

// makeISO builds an ISO 9660 image from the given slash-separated path -> content
// map using xorriso's mkisofs emulation, and returns the image path. This stands
// in for the recovery ISO that pkg/recoverykit produces.
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

// manifestFor builds the optical.Manifest describing files (path -> content).
func manifestFor(files map[string]string) optical.Manifest {
	manifest := make(optical.Manifest, len(files))
	for name, content := range files {
		manifest[name] = sha256Hex(content)
	}

	return manifest
}

// cloneManifest returns a shallow copy so a test can mutate it independently.
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
