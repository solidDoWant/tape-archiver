// Package optical is the physical burn/verify seam over the pinned xorriso
// (libburnia) binary for the optical recovery disc (SPEC §10). It puts every
// hardware-facing optical-disc operation behind a small, testable surface:
//
//   - State   — report the loaded medium's state (blank / appendable write-once /
//     non-blank rewritable / finalized);
//   - WriteImage — burn a prepared, mountable ISO 9660 image onto the disc;
//   - Blank   — reclaim (reformat) a rewritable disc, refusing a write-once one;
//   - Eject   — eject the disc;
//   - Verify  — mount a burned disc read-only and check every file's SHA-256
//     against a manifest.
//
// This is the device/tooling layer the Burn/Verify activities (a later sub-issue
// of parent #98) call; it contains no workflow logic, no Temporal, and no config
// types — only device I/O and os/exec to xorriso (plus mount/umount for the
// read-back). Blank and state detection are unconditional here: this package only
// reports state and performs the requested operation. The overwrite *decision*
// (whether a non-blank disc may be reclaimed) lives in the activity/workflow
// layer, so "never silently overwrite" is enforced there, above this seam.
//
// # Why xorriso, and why shell out
//
// M-DISC is a write-once DVD-R (SPEC §10): the image cannot simply be dd'd to the
// block device — MMC track/session finalization is required to produce a readable
// filesystem, which is why the burn shells out. xorriso is chosen over growisofs
// because its "stdio:" pseudo-drive runs the identical burn code path against a
// regular file or loop device, so the full write/verify path is testable in CI
// with no optical hardware (see driveAddress), and it provides -blank for the
// rewritable-reclaim case. ISO *creation* stays pure-Go (pkg/recoverykit); this
// package is only the physical burn/verify seam.
//
// The burn tool ships in the data-worker image only — it is deliberately NOT on
// the recovery disc, which only needs to *read* ISO 9660 (any DVD drive mounts
// it; SPEC §10).
package optical

import "strings"

// xorrisoBin is the burn binary this package shells out to. It is bundled in the
// data-worker image (nix/data-worker-image.nix) at a pinned version surfaced via
// internal/buildinfo, and present in the dev shell for the integration tests.
const xorrisoBin = "xorriso"

// Disc is a single optical disc addressed by its device path — a real optical
// drive (e.g. /dev/sr0), or, in tests, a regular file or loop device backing a
// pseudo-disc. All operations are methods on Disc so the same code targets real
// hardware and file/loop-device-backed media alike.
type Disc struct {
	// device is the path to the optical drive, backing file, or loop device.
	device string
}

// NewDisc returns a Disc targeting the given device path. The path may be a real
// optical drive (/dev/sr0), or — for tests — a regular file or loop device that
// xorriso drives through its stdio: pseudo-drive (see driveAddress).
func NewDisc(device string) *Disc {
	return &Disc{device: device}
}

// opticalDevicePrefixes are the device-node prefixes that denote a real MMC
// optical drive. xorriso addresses these directly; anything else (a regular file
// or a loop device used to back a pseudo-disc in tests) is driven through the
// stdio: pseudo-drive, which turns the real burn code path loose on file-backed
// media. This is the seam that makes the whole write/verify path testable in CI
// without optical hardware.
var opticalDevicePrefixes = []string{"/dev/sr", "/dev/scd", "/dev/dvd", "/dev/cdrom"}

// isOpticalDevice reports whether device names a real MMC optical drive (rather
// than a file or loop device that must be driven through stdio:).
func isOpticalDevice(device string) bool {
	for _, prefix := range opticalDevicePrefixes {
		if strings.HasPrefix(device, prefix) {
			return true
		}
	}

	return false
}

// driveAddress returns the xorriso drive address for this disc's device: a real
// optical drive is addressed directly, so xorriso uses its MMC backend (real
// track/session finalization); a file or loop device is wrapped in the stdio:
// pseudo-drive, which xorriso treats as random-access, always-overwriteable
// media. On file-backed media the write-once-specific states (a finalized or
// appendable write-once medium) therefore cannot arise — those assertions are
// gated behind real hardware in the tests.
func (d *Disc) driveAddress() string {
	if isOpticalDevice(d.device) {
		return d.device
	}

	return "stdio:" + d.device
}
