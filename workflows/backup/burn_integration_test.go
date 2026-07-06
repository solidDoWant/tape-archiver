//go:build integration

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// discContents stands in for the recovery ISO the Report phase produces (SPEC §10):
// the report, the SHA-256 manifest, the recovery text, an LTFS index, and a recovery
// binary. Slash-separated disc-relative path -> file content.
var discContents = map[string]string{
	"report.pdf":           "pretend PDF report bytes\n",
	"manifest.sha256":      "pretend tape manifest\n",
	"recovery.txt":         "step-by-step recovery procedure\n",
	"bin/age":              "pretend static age binary\n",
	"ltfs-index/T1.schema": "<index>tape one</index>\n",
}

// TestBurnDiscVerifyRoundTrip drives the BurnDisc and VerifyDisc activities against a
// loop-device-backed pseudo-disc through the real xorriso/mount path: a blank disc is
// burned and reads back matching its disc-content manifest (AC1, AC2), and a tampered
// manifest is caught as a burn failure (AC2).
func TestBurnDiscVerifyRoundTrip(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)
	testutil.SkipIfLosetupUnavailable(t)

	device := loopDevice(t, 64<<20) // 64 MiB backing, ample for a tens-of-MB ISO
	isoPath := makeISO(t, discContents)
	manifestPath := writeManifestFile(t, discContents)

	acts := newBurnActivities()

	// --- Burn a blank disc (AC1) ----------------------------------------------
	result, err := acts.BurnDisc(t.Context(), BurnDiscInput{Device: device, ISOPath: isoPath})
	require.NoError(t, err, "burn blank disc")
	assert.Equal(t, device, result.Device)
	assert.False(t, result.OverwroteNonBlank, "a blank disc is not an overwrite")

	// --- Verify the burned disc matches its manifest (AC2) --------------------
	require.NoError(t, acts.VerifyDisc(t.Context(), VerifyDiscInput{
		Device:       device,
		ManifestPath: manifestPath,
	}), "verify burned disc")

	// --- A tampered manifest is a burn failure (AC2) --------------------------
	tampered := cloneContents(discContents)
	tampered["report.pdf"] = "these bytes are NOT what was burned\n"
	tamperedManifest := writeManifestFile(t, tampered)

	err = acts.VerifyDisc(t.Context(), VerifyDiscInput{Device: device, ManifestPath: tamperedManifest})
	require.Error(t, err, "verify must fail when the disc does not match the manifest")
	assert.ErrorContains(t, err, "report.pdf")
}

// TestBurnDiscRefusesNonBlankByDefault asserts that with AllowNonBlankDiscs unset a
// non-blank disc is refused with the typed operator-pause error and left intact — no
// partial or bad disc is produced (AC3).
func TestBurnDiscRefusesNonBlankByDefault(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)
	testutil.SkipIfLosetupUnavailable(t)

	device := loopDevice(t, 64<<20)
	isoPath := makeISO(t, discContents)
	manifestPath := writeManifestFile(t, discContents)

	acts := newBurnActivities()

	// First burn makes the (rewritable) disc non-blank.
	_, err := acts.BurnDisc(t.Context(), BurnDiscInput{Device: device, ISOPath: isoPath})
	require.NoError(t, err, "seed the disc with a first burn")

	// A second burn without the opt-in must refuse it.
	_, err = acts.BurnDisc(t.Context(), BurnDiscInput{Device: device, ISOPath: isoPath})
	require.Error(t, err, "a non-blank disc must be refused by default")
	assert.True(t, IsDiscNotWritable(err), "the refusal must be the typed operator-pause error: %v", err)

	// The disc is untouched: it still matches the first burn's manifest.
	require.NoError(t, acts.VerifyDisc(t.Context(), VerifyDiscInput{
		Device:       device,
		ManifestPath: manifestPath,
	}), "the refused disc must be left intact")
}

// TestBurnDiscReclaimsNonBlankRewritable asserts that with AllowNonBlankDiscs set a
// non-blank rewritable disc is reclaimed and rewritten, the result records the
// overwrite, and the reburned disc verifies (AC4). A loop/stdio disc is always
// rewritable, so it exercises the reclaim path directly.
func TestBurnDiscReclaimsNonBlankRewritable(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)
	testutil.SkipIfLosetupUnavailable(t)

	device := loopDevice(t, 64<<20)
	acts := newBurnActivities()

	// Seed the disc with an unrelated first burn so it is non-blank with different
	// contents than the second burn.
	firstISO := makeISO(t, map[string]string{"stale.txt": "old contents to be reclaimed\n"})
	_, err := acts.BurnDisc(t.Context(), BurnDiscInput{Device: device, ISOPath: firstISO})
	require.NoError(t, err, "seed the disc with a first burn")

	// Reburn the real recovery contents over the non-blank disc, with the opt-in.
	isoPath := makeISO(t, discContents)
	manifestPath := writeManifestFile(t, discContents)

	result, err := acts.BurnDisc(t.Context(), BurnDiscInput{
		Device:             device,
		ISOPath:            isoPath,
		AllowNonBlankDiscs: true,
	})
	require.NoError(t, err, "reclaim and reburn a non-blank rewritable disc")
	assert.True(t, result.OverwroteNonBlank, "the result must record the deliberate overwrite")

	require.NoError(t, acts.VerifyDisc(t.Context(), VerifyDiscInput{
		Device:       device,
		ManifestPath: manifestPath,
	}), "the reburned disc must match the new manifest")
}

// TestBurnDiscRefusesWriteOnceMedium asserts the opt-in never forces a write-once
// overwrite (AC5). A file-backed stdio/loop disc is always rewritable, so this needs
// real write-once media; it is skipped unless OPTICAL_BURN_DEV points at one. The
// decision itself is covered exhaustively by TestDecideBurn.
func TestBurnDiscRefusesWriteOnceMedium(t *testing.T) {
	testutil.SkipIfXorrisoUnavailable(t)

	device := testutil.OpticalBurnDev(t)

	// Guard: the assertion is only meaningful against a non-blank write-once disc.
	state, err := optical.NewDisc(device).State(t.Context())
	require.NoError(t, err, "read state of the configured burn device")
	require.Contains(t, []optical.DiscState{optical.StateAppendableWriteOnce, optical.StateFinalized}, state,
		"%s must point at a non-blank write-once disc for this assertion", testutil.OpticalBurnDevEnv)

	isoPath := makeISO(t, discContents)

	// Even with the opt-in set, a write-once disc must be refused.
	_, err = newBurnActivities().BurnDisc(t.Context(), BurnDiscInput{
		Device:             device,
		ISOPath:            isoPath,
		AllowNonBlankDiscs: true,
	})
	require.Error(t, err, "a write-once disc must be refused even with the opt-in")
	assert.True(t, IsDiscNotWritable(err), "the refusal must be the typed operator-pause error: %v", err)
}

// loopDevice creates a size-byte backing file and attaches it to a loop device,
// returning the loop device path. Both are cleaned up when the test ends. Requires
// root (losetup), which the integration target provides.
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
// map using xorriso's mkisofs emulation, standing in for the recovery ISO that
// pkg/recoverykit produces, and returns the image path.
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
		"-as", "mkisofs", "-o", isoPath, "-V", "BURN_IT", stage).CombinedOutput()
	require.NoError(t, err, "build ISO: %s", strings.TrimSpace(string(out)))

	return isoPath
}

// writeManifestFile writes a sha256sum-format disc-content manifest for files
// (path -> content) to a temp file and returns its path — the on-disk manifest
// VerifyDisc reads and parses.
func writeManifestFile(t *testing.T, files map[string]string) string {
	t.Helper()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	sort.Strings(names)

	var lines []string

	for _, name := range names {
		sum := sha256.Sum256([]byte(files[name]))
		lines = append(lines, fmt.Sprintf("%s  %s", hex.EncodeToString(sum[:]), name))
	}

	path := filepath.Join(t.TempDir(), "disc.sha256")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	return path
}

// cloneContents returns a shallow copy so a test can mutate it independently.
func cloneContents(files map[string]string) map[string]string {
	clone := make(map[string]string, len(files))
	for name, content := range files {
		clone[name] = content
	}

	return clone
}
