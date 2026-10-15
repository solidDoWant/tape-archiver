// Package mhvtl makes an mhvtl virtual library mirror a real library's slot
// inventory, so a dry run rehearses the conditions a real run would meet.
//
// The virtual library is rearranged while its daemons keep running: tapes move
// with MOVE MEDIUM, and enter or leave the library through its I/O station using
// the daemon's own operator commands (see command_linux.go). The daemons are never
// restarted or signalled. A restart renumbers the device nodes passed into the
// worker's container, and SIGHUP rebuilds the library inside a signal handler,
// which corrupts or crashes it (issue #342).
//
// The real library is only ever read.
package mhvtl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

const (
	DefaultConfigDir = "/etc/mhvtl"
	DefaultLibraryID = 10
)

const (
	mhvtlConfFile = "mhvtl.conf"
	homePathKey   = "MHVTL_HOME_PATH"
	// mamFile is the per-volume file vtllibrary and vtltape open to identify and
	// mount a cartridge; a barcode without one cannot be loaded.
	mamFile = "mam"
)

// Library is the virtual changer being rearranged. *tape.Changer implements it.
// Slot numbers are friendly: storage slots first, then I/O slots.
type Library interface {
	Inventory(ctx context.Context) (tape.Inventory, error)
	Transfer(ctx context.Context, srcSlot, dstSlot int) error
	Unload(ctx context.Context, slot, drive int) error
}

// Reprovisioner mirrors a real library onto one mhvtl library. Use New.
type Reprovisioner struct {
	configDir string
	// homeDir pins the media home; empty means read it from mhvtl.conf.
	homeDir   string
	libraryID int
	makeMedia func(ctx context.Context, configDir, homeDir string) error
	// command sends an operator command to the library daemon and returns its
	// reply.
	command func(ctx context.Context, libraryID int, cmd string) (string, error)
}

type Option func(*Reprovisioner)

func WithConfigDir(dir string) Option { return func(r *Reprovisioner) { r.configDir = dir } }

func WithHomeDir(dir string) Option { return func(r *Reprovisioner) { r.homeDir = dir } }

func WithLibraryID(id int) Option { return func(r *Reprovisioner) { r.libraryID = id } }

func New(opts ...Option) *Reprovisioner {
	r := &Reprovisioner{
		configDir: DefaultConfigDir,
		libraryID: DefaultLibraryID,
		makeMedia: runMakeVTLMedia,
		command:   sendCommand,
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Mirror rearranges target so that its storage slots hold exactly the barcodes
// source's do, every mirrored tape is blank, and its drives and I/O station are
// empty. It returns an error naming whatever it could not achieve.
//
// Blankness is not mirrored: element status says which slots are occupied, not
// whether a tape holds data, and slot occupancy is what decides whether a run
// finds the blanks it was configured to use.
func (r *Reprovisioner) Mirror(ctx context.Context, target Library, source tape.Inventory) error {
	homeDir, err := r.resolveHomeDir()
	if err != nil {
		return err
	}

	want := occupiedBarcodes(source)

	inv, err := target.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("read the virtual library inventory: %w", err)
	}

	if err := checkGeometry(inv, want); err != nil {
		return err
	}

	if err := r.clearStation(ctx, inv); err != nil {
		return err
	}

	if inv, err = r.unloadDrives(ctx, target); err != nil {
		return err
	}

	if err := r.exportUnwanted(ctx, target, inv, want); err != nil {
		return err
	}

	// Every wanted tape is now either in its slot or outside the library, and no
	// drive holds a volume open, so all of them can be recreated blank.
	if err := r.regenerateMedia(ctx, homeDir, source, want); err != nil {
		return err
	}

	if err := r.importMissing(ctx, target, want); err != nil {
		return err
	}

	return verify(ctx, target, want)
}

// checkGeometry rejects a virtual library that cannot hold the mirror.
func checkGeometry(inv tape.Inventory, want map[int]string) error {
	if len(inv.IOSlots) == 0 {
		return fmt.Errorf("the virtual library has no I/O station; mirroring moves tapes in and out through it")
	}

	have := make(map[int]bool, len(inv.Slots))
	for _, slot := range inv.Slots {
		have[slot.Address] = true
	}

	var absent []int

	for address := range want {
		if !have[address] {
			absent = append(absent, address)
		}
	}

	if len(absent) > 0 {
		slices.Sort(absent)

		return fmt.Errorf("the virtual library has %d storage slots and lacks slots %v that hold tapes in the real library",
			len(inv.Slots), absent)
	}

	return nil
}

// clearStation removes every tape in the I/O station from the library.
func (r *Reprovisioner) clearStation(ctx context.Context, inv tape.Inventory) error {
	if !slices.ContainsFunc(inv.IOSlots, func(slot tape.IOElement) bool { return slot.Full }) {
		return nil
	}

	return r.station(ctx, "open map", "empty map", "close map")
}

// unloadDrives returns every loaded tape to a storage slot, preferring the one it
// came from. A tape returned to the wrong slot is exported later like any other.
func (r *Reprovisioner) unloadDrives(ctx context.Context, target Library) (tape.Inventory, error) {
	inv, err := target.Inventory(ctx)
	if err != nil {
		return tape.Inventory{}, fmt.Errorf("read the virtual library inventory: %w", err)
	}

	unloaded := false

	for _, drive := range inv.Drives {
		if !drive.Loaded {
			continue
		}

		slot := emptySlot(inv, drive.SourceSlot)
		if slot == 0 {
			return tape.Inventory{}, fmt.Errorf("drive %d holds %s but the virtual library has no empty slot to unload it to",
				drive.Address, drive.Barcode)
		}

		if err := target.Unload(ctx, slot, drive.Address); err != nil {
			return tape.Inventory{}, fmt.Errorf("unload %s from drive %d to slot %d: %w", drive.Barcode, drive.Address, slot, err)
		}

		slog.InfoContext(ctx, "mhvtl: unloaded drive", "drive", drive.Address, "barcode", drive.Barcode, "slot", slot)

		for i := range inv.Slots {
			if inv.Slots[i].Address == slot {
				inv.Slots[i].Full = true
			}
		}

		unloaded = true
	}

	if !unloaded {
		return inv, nil
	}

	if inv, err = target.Inventory(ctx); err != nil {
		return tape.Inventory{}, fmt.Errorf("read the virtual library inventory: %w", err)
	}

	for _, drive := range inv.Drives {
		if drive.Loaded {
			return tape.Inventory{}, fmt.Errorf("drive %d still holds %s after unloading it", drive.Address, drive.Barcode)
		}
	}

	return inv, nil
}

// emptySlot returns preferred if it is an empty storage slot, else the first empty
// storage slot, else 0.
func emptySlot(inv tape.Inventory, preferred int) int {
	first := 0

	for _, slot := range inv.Slots {
		if slot.Full {
			continue
		}

		if slot.Address == preferred {
			return preferred
		}

		if first == 0 {
			first = slot.Address
		}
	}

	return first
}

// exportUnwanted removes every tape that is not already in the slot the mirror
// wants it in, a station-load at a time.
func (r *Reprovisioner) exportUnwanted(ctx context.Context, target Library, inv tape.Inventory, want map[int]string) error {
	var unwanted []tape.StorageElement

	for _, slot := range inv.Slots {
		if slot.Full && want[slot.Address] != string(slot.Barcode) {
			unwanted = append(unwanted, slot)
		}
	}

	for batch := range slices.Chunk(unwanted, len(inv.IOSlots)) {
		for i, slot := range batch {
			if err := target.Transfer(ctx, slot.Address, inv.IOSlots[i].Address); err != nil {
				return fmt.Errorf("export %s from slot %d to I/O slot %d: %w",
					slot.Barcode, slot.Address, inv.IOSlots[i].Address, err)
			}
		}

		if err := r.station(ctx, "open map", "empty map", "close map"); err != nil {
			return err
		}
	}

	if len(unwanted) > 0 {
		slog.InfoContext(ctx, "mhvtl: exported tapes the mirror does not want", "count", len(unwanted))
	}

	return nil
}

// regenerateMedia writes library_contents to match the mirror and recreates
// every wanted volume blank. library_contents is also what vtllibrary reads if
// it is ever restarted, so it is kept true to the live state.
func (r *Reprovisioner) regenerateMedia(ctx context.Context, homeDir string, source tape.Inventory, want map[int]string) error {
	path := filepath.Join(r.configDir, "library_contents."+strconv.Itoa(r.libraryID))
	if err := os.WriteFile(path, []byte(RenderLibraryContents(source)), 0o644); err != nil {
		return fmt.Errorf("write mhvtl library contents %s: %w", path, err)
	}

	if err := r.makeMedia(ctx, r.configDir, homeDir); err != nil {
		return fmt.Errorf("regenerate mhvtl media under %s: %w", homeDir, err)
	}

	// make_vtl_media exits 0 even when it creates nothing (issue #339).
	var missing []string

	for _, barcode := range want {
		if _, err := os.Stat(filepath.Join(homeDir, barcode, mamFile)); err != nil {
			missing = append(missing, barcode)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)

		return fmt.Errorf("make_vtl_media reported success but left %d of %d mirrored barcodes with no %s under %s: %s",
			len(missing), len(want), mamFile, homeDir, strings.Join(missing, ", "))
	}

	return nil
}

// importMissing inserts each wanted tape that is not in the library through the
// I/O station and moves it to its slot.
func (r *Reprovisioner) importMissing(ctx context.Context, target Library, want map[int]string) error {
	inv, err := target.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("read the virtual library inventory: %w", err)
	}

	full := make(map[int]bool, len(inv.Slots))
	for _, slot := range inv.Slots {
		full[slot.Address] = slot.Full
	}

	var missing []int

	for address := range want {
		if !full[address] {
			missing = append(missing, address)
		}
	}

	slices.Sort(missing)

	for batch := range slices.Chunk(missing, len(inv.IOSlots)) {
		commands := []string{"open map"}
		for _, address := range batch {
			commands = append(commands, "load map "+want[address])
		}

		if err := r.station(ctx, append(commands, "close map")...); err != nil {
			return err
		}

		station, err := target.Inventory(ctx)
		if err != nil {
			return fmt.Errorf("read the virtual library inventory: %w", err)
		}

		for _, address := range batch {
			index := slices.IndexFunc(station.IOSlots, func(slot tape.IOElement) bool {
				return slot.Full && string(slot.Barcode) == want[address]
			})
			if index < 0 {
				return fmt.Errorf("inserted %s into the I/O station but the library does not report it there", want[address])
			}

			from := station.IOSlots[index].Address
			if err := target.Transfer(ctx, from, address); err != nil {
				return fmt.Errorf("move %s from I/O slot %d to slot %d: %w", want[address], from, address, err)
			}
		}
	}

	if len(missing) > 0 {
		slog.InfoContext(ctx, "mhvtl: imported mirrored tapes", "count", len(missing))
	}

	return nil
}

// verify checks the library now reports exactly the mirrored state.
func verify(ctx context.Context, target Library, want map[int]string) error {
	inv, err := target.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("read the virtual library inventory: %w", err)
	}

	var problems []string

	for _, drive := range inv.Drives {
		if drive.Loaded {
			problems = append(problems, fmt.Sprintf("drive %d holds %s", drive.Address, drive.Barcode))
		}
	}

	for _, slot := range inv.IOSlots {
		if slot.Full {
			problems = append(problems, fmt.Sprintf("I/O slot %d holds %s", slot.Address, slot.Barcode))
		}
	}

	for _, slot := range inv.Slots {
		got := ""
		if slot.Full {
			got = string(slot.Barcode)
		}

		if got != want[slot.Address] {
			problems = append(problems, fmt.Sprintf("slot %d holds %q, want %q", slot.Address, got, want[slot.Address]))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("the virtual library does not match the mirror: %s", strings.Join(problems, "; "))
	}

	return nil
}

// station sends I/O station commands in order, failing on the first reply that
// is not "OK".
func (r *Reprovisioner) station(ctx context.Context, commands ...string) error {
	for _, cmd := range commands {
		reply, err := r.command(ctx, r.libraryID, cmd)
		if err != nil {
			return fmt.Errorf("mhvtl library %d %q: %w", r.libraryID, cmd, err)
		}

		if reply != "OK" {
			return fmt.Errorf("mhvtl library %d %q: %s", r.libraryID, cmd, reply)
		}
	}

	return nil
}

// resolveHomeDir returns the media home: the pinned one, else MHVTL_HOME_PATH
// from mhvtl.conf. It must already exist — in the container it is a bind mount,
// and creating it would put media where vtllibrary is not looking (issue #335).
func (r *Reprovisioner) resolveHomeDir() (string, error) {
	homeDir := r.homeDir
	source := "the configured media home"

	if homeDir == "" {
		confPath := filepath.Join(r.configDir, mhvtlConfFile)

		var err error
		if homeDir, err = readHomePath(confPath); err != nil {
			return "", err
		}

		source = homePathKey + " in " + confPath
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

// readHomePath extracts MHVTL_HOME_PATH from mhvtl.conf, a shell fragment: it
// allows indentation, export, quotes and comments, and the last assignment wins.
// Values needing shell expansion are rejected.
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

		if key == homePathKey {
			found = strings.Trim(strings.TrimSpace(value), `"'`)
		}
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

// RenderLibraryContents renders an mhvtl library_contents file whose storage
// slots mirror source. The header's "Drive N:" lines name drive serials, not
// tapes, so they are left blank; drive and I/O station occupancy cannot be
// expressed in this file.
func RenderLibraryContents(source tape.Inventory) string {
	var b strings.Builder

	b.WriteString("Drive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\nMAP 2:\nMAP 3:\n\n")

	slots := slices.Clone(source.Slots)
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

// runMakeVTLMedia (re)creates blank media for every barcode in library_contents.
// -c is not passed: a missing home must fail, not be created (issue #335).
func runMakeVTLMedia(ctx context.Context, configDir, homeDir string) error {
	cmd := exec.CommandContext(ctx, "make_vtl_media", "-H", homeDir, "-C", configDir, "-f")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("make_vtl_media: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// occupiedBarcodes maps slot address to barcode for every storage slot holding a
// labelled tape. A barcodeless tape cannot be mirrored and counts as empty.
func occupiedBarcodes(inv tape.Inventory) map[int]string {
	occupied := make(map[int]string, len(inv.Slots))

	for _, slot := range inv.Slots {
		if slot.Full && slot.Barcode != "" {
			occupied[slot.Address] = string(slot.Barcode)
		}
	}

	return occupied
}
