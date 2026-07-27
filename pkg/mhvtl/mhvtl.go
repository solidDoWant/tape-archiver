// Package mhvtl reprovisions an mhvtl virtual tape library so its slot
// inventory mirrors a real library's, making a dry run a faithful rehearsal of
// the real thing (SPEC §4.1 dry-run).
//
// Without this, the two libraries drift: a virtual library stocked with a full
// complement of blanks passes a dry run that the real library — short of blanks,
// or with tapes parked in drives from an earlier run — would fail. Mirroring
// moves that failure into the rehearsal, where it is supposed to surface.
//
// It works entirely on mhvtl's own state: it rewrites library_contents.<id>,
// regenerates the backing media with make_vtl_media (bundled in the data-worker
// image, so no mhvtl userspace is needed on the host), and sends SIGHUP to the
// vtllibrary daemon. SIGHUP makes vtllibrary re-read library_contents in place —
// see rereadconfig() in mhvtl's usr/cmd/vtllibrary.c, which takes the unit
// offline, tears it down, and re-initialises it from the config file. The daemon
// is deliberately NOT restarted and the kernel module NOT reloaded: both would
// destroy and recreate /dev/sch*, /dev/st*, and /dev/sg*, invalidating the device
// nodes the worker's container had passed through to it at start.
//
// Nothing here issues destructive SCSI. The real library named as the mirror
// source is only ever read (READ ELEMENT STATUS), so a misconfigured mirror
// cannot damage real media — the reprovision touches mhvtl's files and its own
// daemon and nothing else.
package mhvtl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Default locations and identifiers, matching mhvtl's own conventions
// (MHVTL_HOME_PATH in /etc/mhvtl/mhvtl.conf) and the library ID this project
// configures in device.conf.
const (
	DefaultConfigDir = "/etc/mhvtl"
	DefaultHomeDir   = "/opt/mhvtl"
	DefaultLibraryID = 10
)

// reloadPollInterval and reloadTimeout bound the wait for vtllibrary to finish
// re-reading its config. rereadconfig() reports the unit offline while it
// re-initialises, so the changer answers SCSI again only once it is done.
const (
	reloadPollInterval = 250 * time.Millisecond
	reloadTimeout      = 60 * time.Second
)

// Reprovisioner rewrites one mhvtl library's contents to mirror a real library.
// The zero value is not usable; use New.
type Reprovisioner struct {
	configDir string
	homeDir   string
	libraryID int
	// makeMedia runs make_vtl_media; overridable in tests.
	makeMedia func(ctx context.Context, configDir, homeDir string) error
	// signal delivers SIGHUP to a pid; overridable in tests.
	signal func(pid int) error
	// findDaemon locates the vtllibrary process for a library ID; overridable in
	// tests.
	findDaemon func(libraryID int) (int, error)
}

// Option customises a Reprovisioner.
type Option func(*Reprovisioner)

// WithConfigDir overrides the mhvtl config directory (default DefaultConfigDir).
func WithConfigDir(dir string) Option { return func(r *Reprovisioner) { r.configDir = dir } }

// WithHomeDir overrides the mhvtl media home (default DefaultHomeDir).
func WithHomeDir(dir string) Option { return func(r *Reprovisioner) { r.homeDir = dir } }

// WithLibraryID overrides the mhvtl library ID (default DefaultLibraryID).
func WithLibraryID(id int) Option { return func(r *Reprovisioner) { r.libraryID = id } }

// New builds a Reprovisioner with mhvtl's default paths, overridable by opts.
func New(opts ...Option) *Reprovisioner {
	r := &Reprovisioner{
		configDir:  DefaultConfigDir,
		homeDir:    DefaultHomeDir,
		libraryID:  DefaultLibraryID,
		makeMedia:  runMakeVTLMedia,
		signal:     func(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) },
		findDaemon: findVTLLibraryPID,
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Mirror makes the virtual library's storage slots match source: every slot that
// holds a tape in source holds one here, under the same barcode, and every slot
// empty there is empty here. Drives and the I/O station are left empty — a
// rehearsal starts from tapes at rest, and the Load phase moves them itself.
//
// The media are (re)created blank. Blankness is not mirrored: the real library's
// element status reports which slots are occupied but not whether a tape has
// data, and determining that would mean loading and mounting every tape. Slot
// occupancy is the property that actually decides whether a run can find the
// blanks it was configured to use, which is the failure this exists to surface.
func (r *Reprovisioner) Mirror(ctx context.Context, source tape.Inventory) error {
	contents := RenderLibraryContents(source)

	path := r.contentsPath()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write mhvtl library contents %s: %w", path, err)
	}

	slog.InfoContext(ctx, "mhvtl: wrote mirrored library contents",
		"path", path, "occupiedSlots", countFull(source))

	if err := r.makeMedia(ctx, r.configDir, r.homeDir); err != nil {
		return fmt.Errorf("regenerate mhvtl media: %w", err)
	}

	pid, err := r.findDaemon(r.libraryID)
	if err != nil {
		return err
	}

	if err := r.signal(pid); err != nil {
		return fmt.Errorf("signal vtllibrary (pid %d) to re-read its config: %w", pid, err)
	}

	slog.InfoContext(ctx, "mhvtl: signalled vtllibrary to re-read contents", "pid", pid)

	return nil
}

// contentsPath is the library_contents file for this library ID.
func (r *Reprovisioner) contentsPath() string {
	return filepath.Join(r.configDir, "library_contents."+strconv.Itoa(r.libraryID))
}

// WaitReady blocks until the changer answers with the mirrored slot occupancy,
// or ctx/timeout expires. vtllibrary reports the unit offline while
// rereadconfig() re-initialises it, so a changer that answers with the expected
// contents is the signal that the reload landed — polling for mere reachability
// would race and let the tape path start against the pre-reload slot map.
func (r *Reprovisioner) WaitReady(ctx context.Context, changer *tape.Changer, want tape.Inventory) error {
	deadline := time.Now().Add(reloadTimeout)
	wantOccupied := occupiedBarcodes(want)

	var lastErr error

	for {
		inv, err := changer.Inventory(ctx)
		if err == nil {
			if got := occupiedBarcodes(inv); equalStringMaps(got, wantOccupied) {
				return nil
			}

			lastErr = fmt.Errorf("changer reports %d occupied slots, want %d",
				len(occupiedBarcodes(inv)), len(wantOccupied))
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the mhvtl library to reload: %w",
				reloadTimeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the mhvtl library to reload: %w", ctx.Err())
		case <-time.After(reloadPollInterval):
		}
	}
}

// RenderLibraryContents renders an mhvtl library_contents file mirroring source.
// The header block (drives, picker, MAPs) is emitted empty and the storage slots
// follow in ascending address order, each naming the barcode in the corresponding
// source slot or nothing when that slot is empty. This is the format vtllibrary's
// init_slot_info parses.
func RenderLibraryContents(source tape.Inventory) string {
	var b strings.Builder

	// Drives and I/O slots start empty: a rehearsal begins with every tape at
	// rest in a storage slot, and the Load phase is what moves them.
	b.WriteString("Drive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\nMAP 2:\nMAP 3:\n\n")

	slots := append([]tape.StorageElement(nil), source.Slots...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].Address < slots[j].Address })

	for _, slot := range slots {
		if slot.Full && slot.Barcode != "" {
			fmt.Fprintf(&b, "Slot %d: %s\n", slot.Address, slot.Barcode)

			continue
		}

		fmt.Fprintf(&b, "Slot %d:\n", slot.Address)
	}

	return b.String()
}

// runMakeVTLMedia creates any media named by library_contents that does not
// already exist. mhvtl refuses to load a barcode with no backing media file, so
// mirroring a real library's barcodes requires materialising them here.
func runMakeVTLMedia(ctx context.Context, configDir, homeDir string) error {
	cmd := exec.CommandContext(ctx, "make_vtl_media",
		"-H", homeDir,
		"-C", configDir,
		"-c", // create media for every barcode in library_contents
		"-f", // overwrite stale media from a previous run, so tapes start blank
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("make_vtl_media: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// findVTLLibraryPID locates the running vtllibrary daemon for libraryID by
// scanning /proc for its "-q<id>" argument. The daemon runs on the host, outside
// the worker's container, so this requires the container to share the host PID
// namespace (--pid=host in the data-worker unit).
func findVTLLibraryPID(libraryID int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc to find the vtllibrary daemon: %w", err)
	}

	want := "-q" + strconv.Itoa(libraryID)

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}

		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) == 0 || filepath.Base(args[0]) != "vtllibrary" {
			continue
		}

		for _, arg := range args[1:] {
			if arg == want {
				return pid, nil
			}
		}
	}

	return 0, fmt.Errorf("no running vtllibrary daemon found for library %d: %w", libraryID, ErrDaemonNotFound)
}

// ErrDaemonNotFound reports that no vtllibrary process was found for the library.
// It usually means the worker's container does not share the host PID namespace
// (--pid=host), or mhvtl is not running on this host.
var ErrDaemonNotFound = errors.New("vtllibrary daemon not found")

// occupiedBarcodes maps slot address to barcode for every occupied storage slot.
func occupiedBarcodes(inv tape.Inventory) map[int]string {
	occupied := make(map[int]string, len(inv.Slots))

	for _, slot := range inv.Slots {
		if slot.Full {
			occupied[slot.Address] = string(slot.Barcode)
		}
	}

	return occupied
}

// countFull returns how many storage slots hold a tape.
func countFull(inv tape.Inventory) int { return len(occupiedBarcodes(inv)) }

// equalStringMaps reports whether two slot->barcode maps are identical.
func equalStringMaps(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valueA := range a {
		if valueB, ok := b[key]; !ok || valueA != valueB {
			return false
		}
	}

	return true
}
