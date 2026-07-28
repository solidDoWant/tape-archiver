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

// Default locations and identifiers, matching mhvtl's own conventions and the
// library ID this project configures in device.conf.
//
// There is deliberately no default media home. Where mhvtl keeps its volumes is a
// per-host decision — this project's storage host puts them on the bulk pool,
// because a dry run writes real volumes and would otherwise fill the root disk —
// and mhvtl.conf is where a host declares it. Compiling in a guess produced a
// worker that created a full set of media somewhere the library was not looking
// (issue #335), so the path is read from mhvtl.conf instead; see resolveHomeDir.
const (
	DefaultConfigDir = "/etc/mhvtl"
	DefaultLibraryID = 10
)

// mhvtlConfFile is mhvtl's own settings file within the config directory, and
// homePathKey the shell assignment in it that names the media home. make_vtl_media
// reads the same key from the same file (its CONFIG_FILE, redirected by -C).
const (
	mhvtlConfFile = "mhvtl.conf"
	homePathKey   = "MHVTL_HOME_PATH"
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
	// homeDir pins the media home; empty means resolve it from mhvtl.conf.
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

// WithHomeDir pins the mhvtl media home instead of reading it from mhvtl.conf.
func WithHomeDir(dir string) Option { return func(r *Reprovisioner) { r.homeDir = dir } }

// WithLibraryID overrides the mhvtl library ID (default DefaultLibraryID).
func WithLibraryID(id int) Option { return func(r *Reprovisioner) { r.libraryID = id } }

// New builds a Reprovisioner with mhvtl's default paths, overridable by opts. The
// media home is left unset and resolved from mhvtl.conf at Mirror time unless
// WithHomeDir pins it.
func New(opts ...Option) *Reprovisioner {
	r := &Reprovisioner{
		configDir:  DefaultConfigDir,
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
	// Resolve the media home before touching anything: a home that cannot be
	// resolved makes the whole reprovision pointless, and failing first leaves the
	// library's existing contents intact rather than half-rewritten.
	homeDir, err := r.resolveHomeDir()
	if err != nil {
		return err
	}

	contents := RenderLibraryContents(source)

	path := r.contentsPath()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write mhvtl library contents %s: %w", path, err)
	}

	slog.InfoContext(ctx, "mhvtl: wrote mirrored library contents",
		"path", path, "occupiedSlots", countFull(source), "mediaHome", homeDir)

	if err := r.makeMedia(ctx, r.configDir, homeDir); err != nil {
		return fmt.Errorf("regenerate mhvtl media under %s: %w", homeDir, err)
	}

	if err := verifyMedia(homeDir, source); err != nil {
		return err
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

// confPath is mhvtl's own settings file within the config directory.
func (r *Reprovisioner) confPath() string {
	return filepath.Join(r.configDir, mhvtlConfFile)
}

// resolveHomeDir returns the directory mhvtl keeps its media in: the pinned one if
// WithHomeDir set it, otherwise MHVTL_HOME_PATH from mhvtl.conf.
//
// It insists the directory already exists. The worker runs in a container and
// reaches the media home through a bind mount, so a missing directory means the
// mount is absent or the declared path is wrong — and creating it would produce
// exactly the failure this resolution exists to prevent: a full set of media
// written somewhere vtllibrary is not looking, a mirror that reports success, and
// a run that dies later at Load with slots that should have held tapes.
func (r *Reprovisioner) resolveHomeDir() (string, error) {
	homeDir := r.homeDir
	source := "the configured media home"

	if homeDir == "" {
		var err error

		homeDir, err = readHomePath(r.confPath())
		if err != nil {
			return "", err
		}

		source = homePathKey + " in " + r.confPath()
	}

	info, err := os.Stat(homeDir)
	if err != nil {
		return "", fmt.Errorf("stat the mhvtl media home %s (%s): %w", homeDir, source, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("the mhvtl media home %s (%s) is not a directory", homeDir, source)
	}

	return homeDir, nil
}

// readHomePath extracts MHVTL_HOME_PATH from an mhvtl.conf. The file is a shell
// fragment mhvtl sources, so this reads it the way sourcing would: assignments may
// be indented or exported, '#' starts a comment, the value may be quoted, and a
// later assignment wins over an earlier one.
//
// A value containing '$' is rejected rather than passed through, because expanding
// it would mean interpreting the surrounding shell — and a path that silently
// keeps a literal '$var' in it is the same class of bug as guessing the path.
func readHomePath(confPath string) (string, error) {
	contents, err := os.ReadFile(confPath)
	if err != nil {
		return "", fmt.Errorf("read %s to find the mhvtl media home (%s): %w", confPath, homePathKey, err)
	}

	found := ""

	for line := range strings.Lines(string(contents)) {
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = line[:comment]
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		if rest, exported := strings.CutPrefix(key, "export "); exported {
			key = strings.TrimSpace(rest)
		}

		if key != homePathKey {
			continue
		}

		found = strings.Trim(strings.TrimSpace(value), `"'`)
	}

	if found == "" {
		return "", fmt.Errorf("%s declares no %s, so there is nowhere to write mhvtl media", confPath, homePathKey)
	}

	if strings.Contains(found, "$") {
		return "", fmt.Errorf("%s sets %s to %q, which this does not expand; set a literal path",
			confPath, homePathKey, found)
	}

	return found, nil
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

// runMakeVTLMedia creates the media named by library_contents. mhvtl refuses to
// load a barcode with no backing media file, so mirroring a real library's
// barcodes requires materialising them here.
//
// -c (--create-dir, mkdir the home) is deliberately NOT passed: the home is
// resolved and checked by resolveHomeDir, and letting make_vtl_media conjure a
// missing one is what turned a wrong path into a silent success (issue #335).
func runMakeVTLMedia(ctx context.Context, configDir, homeDir string) error {
	cmd := exec.CommandContext(ctx, "make_vtl_media",
		"-H", homeDir, // write media here, not wherever mhvtl.conf's default points
		"-C", configDir, // read device.conf and mhvtl.conf from here
		"-f", // overwrite stale media from a previous run, so tapes start blank
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("make_vtl_media: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// mamFile is the per-volume file vtllibrary opens to identify a cartridge
// (get_cart_type in usr/cmd/vtllibrary.c) and vtltape opens to mount it. Its
// absence is exactly what a barcode with no backing media looks like to mhvtl.
const mamFile = "mam"

// verifyMedia insists that every barcode the mirror just published has backing
// media under homeDir, and names the ones that do not.
//
// make_vtl_media's exit status does not carry this. It is a shell script that
// finds the libraries and their barcodes with awk, so on a PATH without awk both
// loops iterate over nothing, no media are created, and the script still exits 0
// (issue #339). Nothing downstream notices either: vtllibrary derives element
// status from library_contents alone, so WaitReady sees the mirrored slots and
// the changer reports tapes that cannot be mounted. The run then dies at Load
// with a MOVE MEDIUM hardware error naming a slot that looks perfectly occupied.
//
// So the invariant Load depends on is checked where it is established, against
// mhvtl's own on-disk layout rather than a tool's return code.
func verifyMedia(homeDir string, source tape.Inventory) error {
	barcodes := occupiedBarcodes(source)

	var missing []string

	for _, barcode := range barcodes {
		if _, err := os.Stat(filepath.Join(homeDir, barcode, mamFile)); err != nil {
			missing = append(missing, barcode)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	sort.Strings(missing)

	return fmt.Errorf(
		"make_vtl_media reported success but left %d of %d mirrored barcodes with no media under %s "+
			"(expected %s/<barcode>/%s): %s; it exits 0 when the tools it shells out to are missing, "+
			"so check that awk is on its PATH",
		len(missing), len(barcodes), homeDir, homeDir, mamFile, strings.Join(missing, ", "))
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
