package mhvtl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// fakeLibrary models an mhvtl library: MOVE MEDIUM may not touch the I/O station
// while it is open, and the operator commands only work while it is open.
type fakeLibrary struct {
	slots   map[int]string // storage address -> barcode, "" empty
	io      []string       // I/O station, index i is friendly slot numStorage+1+i
	drives  []string
	sources []int // per drive, the slot its tape came from
	open    bool
	// stuckDrive, when >= 0, ignores unloads from that drive.
	stuckDrive int
	commands   []string
}

func newFakeLibrary(storage, ioSlots, drives int) *fakeLibrary {
	lib := &fakeLibrary{
		slots:      map[int]string{},
		io:         make([]string, ioSlots),
		drives:     make([]string, drives),
		sources:    make([]int, drives),
		stuckDrive: -1,
	}
	for address := 1; address <= storage; address++ {
		lib.slots[address] = ""
	}

	return lib
}

func (l *fakeLibrary) Inventory(context.Context) (tape.Inventory, error) {
	var inv tape.Inventory

	for index, barcode := range l.drives {
		inv.Drives = append(inv.Drives, tape.DriveElement{
			Address: index, Loaded: barcode != "", Barcode: tape.Barcode(barcode), SourceSlot: l.sources[index],
		})
	}

	for address := 1; address <= len(l.slots); address++ {
		inv.Slots = append(inv.Slots, tape.StorageElement{
			Address: address, Full: l.slots[address] != "", Barcode: tape.Barcode(l.slots[address]),
		})
	}

	for index, barcode := range l.io {
		inv.IOSlots = append(inv.IOSlots, tape.IOElement{
			Address: len(l.slots) + 1 + index, Full: barcode != "", Barcode: tape.Barcode(barcode),
		})
	}

	return inv, nil
}

// element returns a pointer to the barcode held at a friendly slot number.
func (l *fakeLibrary) element(slot int) (*string, error) {
	if slot >= 1 && slot <= len(l.slots) {
		barcode := l.slots[slot]

		return &barcode, nil
	}

	if index := slot - len(l.slots) - 1; index >= 0 && index < len(l.io) {
		if l.open {
			return nil, errors.New("I/O station is open")
		}

		return &l.io[index], nil
	}

	return nil, fmt.Errorf("no slot %d", slot)
}

func (l *fakeLibrary) set(slot int, barcode string) {
	if slot <= len(l.slots) {
		l.slots[slot] = barcode
	} else {
		l.io[slot-len(l.slots)-1] = barcode
	}
}

func (l *fakeLibrary) Transfer(_ context.Context, src, dst int) error {
	from, err := l.element(src)
	if err != nil {
		return err
	}

	to, err := l.element(dst)
	if err != nil {
		return err
	}

	if *from == "" || *to != "" {
		return fmt.Errorf("move %d -> %d: source empty or destination full", src, dst)
	}

	barcode := *from

	l.set(src, "")
	l.set(dst, barcode)

	return nil
}

func (l *fakeLibrary) Unload(_ context.Context, slot, drive int) error {
	if l.slots[slot] != "" || l.drives[drive] == "" {
		return fmt.Errorf("unload drive %d -> %d: drive empty or slot full", drive, slot)
	}

	if drive == l.stuckDrive {
		return nil
	}

	l.slots[slot], l.drives[drive] = l.drives[drive], ""

	return nil
}

func (l *fakeLibrary) inLibrary(barcode string) bool {
	for _, held := range l.slots {
		if held == barcode {
			return true
		}
	}

	return slices.Contains(l.io, barcode) || slices.Contains(l.drives, barcode)
}

func (l *fakeLibrary) command(_ context.Context, libraryID int, cmd string) (string, error) {
	l.commands = append(l.commands, cmd)

	if libraryID != DefaultLibraryID {
		return "", fmt.Errorf("library %d", libraryID)
	}

	switch {
	case cmd == "open map":
		l.open = true
	case cmd == "close map":
		l.open = false
	case cmd == "empty map":
		if !l.open {
			return "Can't empty map while MAP is closed", nil
		}

		clear(l.io)
	case len(cmd) > 9 && cmd[:9] == "load map ":
		barcode := cmd[9:]

		if !l.open {
			return "MAP not opened", nil
		}

		if l.inLibrary(barcode) {
			return "barcode already in library", nil
		}

		index := slices.Index(l.io, "")
		if index < 0 {
			return "MAP Full", nil
		}

		l.io[index] = barcode
	default:
		return "", fmt.Errorf("unexpected command %q", cmd)
	}

	return "OK", nil
}

// source builds a real-library inventory from slot address -> barcode.
func source(storage int, barcodes map[int]string) tape.Inventory {
	var inv tape.Inventory
	for address := 1; address <= storage; address++ {
		inv.Slots = append(inv.Slots, tape.StorageElement{
			Address: address, Full: barcodes[address] != "", Barcode: tape.Barcode(barcodes[address]),
		})
	}

	return inv
}

// materialise does what a working make_vtl_media does: give each barcode a mam.
func materialise(t *testing.T, homeDir string, barcodes ...string) {
	t.Helper()

	for _, barcode := range barcodes {
		dir := filepath.Join(homeDir, barcode)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, mamFile), nil, 0o644))
	}
}

func newTestReprovisioner(t *testing.T, lib *fakeLibrary, want map[int]string) (*Reprovisioner, string, string) {
	t.Helper()

	configDir, homeDir := t.TempDir(), t.TempDir()

	r := New(WithConfigDir(configDir), WithHomeDir(homeDir))
	r.command = lib.command
	r.makeMedia = func(context.Context, string, string) error {
		for _, barcode := range want {
			materialise(t, homeDir, barcode)
		}

		return nil
	}

	return r, configDir, homeDir
}

func TestMirror(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(lib *fakeLibrary)
		want  map[int]string
	}{
		{
			name: "fresh library with different barcodes",
			setup: func(lib *fakeLibrary) {
				for address := 1; address <= 8; address++ {
					lib.slots[address] = fmt.Sprintf("TA%04dL6", address)
				}
			},
			want: map[int]string{2: "REAL02L5", 5: "REAL05L5", 7: "REAL07L5"},
		},
		{
			name: "previous dry run left tapes in drives and the I/O station",
			setup: func(lib *fakeLibrary) {
				lib.slots[1] = "REAL01L5"
				lib.drives[0], lib.sources[0] = "REAL02L5", 2
				lib.drives[1], lib.sources[1] = "REAL03L5", 3
				lib.io[0], lib.io[1] = "WRIT01L5", "WRIT02L5"
			},
			want: map[int]string{1: "REAL01L5", 2: "REAL02L5", 3: "REAL03L5", 4: "REAL04L5"},
		},
		{
			name: "a wanted tape sits in the wrong slot",
			setup: func(lib *fakeLibrary) {
				lib.slots[1] = "REAL02L5"
				lib.slots[2] = "REAL01L5"
			},
			want: map[int]string{1: "REAL01L5", 2: "REAL02L5"},
		},
		{
			name: "more tapes to move than the I/O station holds",
			setup: func(lib *fakeLibrary) {
				for address := 1; address <= 8; address++ {
					lib.slots[address] = fmt.Sprintf("OLD%03dL6", address)
				}
			},
			want: map[int]string{1: "N1L5", 2: "N2L5", 3: "N3L5", 4: "N4L5", 5: "N5L5", 6: "N6L5", 7: "N7L5"},
		},
		{
			name: "the real library is empty",
			setup: func(lib *fakeLibrary) {
				lib.slots[3] = "OLD003L6"
			},
			want: map[int]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			lib := newFakeLibrary(8, 3, 2)
			test.setup(lib)

			r, configDir, _ := newTestReprovisioner(t, lib, test.want)

			require.NoError(t, r.Mirror(t.Context(), lib, source(8, test.want)))

			for address := 1; address <= 8; address++ {
				assert.Equal(t, test.want[address], lib.slots[address], "slot %d", address)
			}

			assert.Equal(t, []string{"", ""}, lib.drives, "drives must be empty")
			assert.Equal(t, []string{"", "", ""}, lib.io, "I/O station must be empty")
			assert.False(t, lib.open, "I/O station must be left closed")

			contents, err := os.ReadFile(filepath.Join(configDir, "library_contents.10"))
			require.NoError(t, err)
			assert.Equal(t, RenderLibraryContents(source(8, test.want)), string(contents))
		})
	}
}

func TestMirrorFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(lib *fakeLibrary)
		want      map[int]string
		storage   int
		makeMedia func(t *testing.T, homeDir string) error
		wantError string
	}{
		{
			name:      "a drive that will not unload is named",
			setup:     func(lib *fakeLibrary) { lib.drives[1], lib.stuckDrive = "STUCK1L5", 1 },
			want:      map[int]string{1: "REAL01L5"},
			wantError: "drive 1 still holds STUCK1L5",
		},
		{
			name:      "the real library has slots the virtual one lacks",
			want:      map[int]string{9: "REAL09L5"},
			storage:   9,
			wantError: "lacks slots [9]",
		},
		{
			name: "make_vtl_media fails",
			want: map[int]string{1: "REAL01L5"},
			makeMedia: func(*testing.T, string) error {
				return errors.New("boom")
			},
			wantError: "regenerate mhvtl media",
		},
		{
			name: "make_vtl_media leaves barcodes without media",
			want: map[int]string{1: "AAA01L5", 2: "BBB02L5", 3: "CCC03L5"},
			makeMedia: func(t *testing.T, homeDir string) error {
				materialise(t, homeDir, "BBB02L5")

				return nil
			},
			wantError: "left 2 of 3 mirrored barcodes with no mam",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			lib := newFakeLibrary(8, 3, 2)
			if test.setup != nil {
				test.setup(lib)
			}

			r, _, homeDir := newTestReprovisioner(t, lib, test.want)
			if test.makeMedia != nil {
				r.makeMedia = func(context.Context, string, string) error { return test.makeMedia(t, homeDir) }
			}

			storage := test.storage
			if storage == 0 {
				storage = 8
			}

			err := r.Mirror(t.Context(), lib, source(storage, test.want))

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantError)

			for _, cmd := range lib.commands {
				assert.NotContains(t, cmd, "load map", "nothing may be imported after a failure")
			}
		})
	}
}

func TestMirrorNamesBarcodesWithoutMedia(t *testing.T) {
	t.Parallel()

	lib := newFakeLibrary(8, 3, 2)
	want := map[int]string{1: "AAA01L5", 2: "BBB02L5", 3: "CCC03L5"}

	r, _, homeDir := newTestReprovisioner(t, lib, want)
	r.makeMedia = func(context.Context, string, string) error {
		materialise(t, homeDir, "BBB02L5")

		return nil
	}

	err := r.Mirror(t.Context(), lib, source(8, want))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AAA01L5, CCC03L5")
}

func TestMirrorSurfacesARejectedStationCommand(t *testing.T) {
	t.Parallel()

	lib := newFakeLibrary(8, 3, 2)
	want := map[int]string{1: "REAL01L5"}
	lib.slots[1] = "REAL01L5"
	lib.slots[2] = "INLIB2L5"

	r, _, _ := newTestReprovisioner(t, lib, want)
	r.command = func(context.Context, int, string) (string, error) { return "MAP not opened", nil }

	err := r.Mirror(t.Context(), lib, source(8, want))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MAP not opened")
}

func TestMirrorRefusesAnUnusableMediaHome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the config dir and the WithHomeDir override ("" for none).
		setup     func(t *testing.T) (configDir, homeDir string)
		wantError string
	}{
		{
			name:      "no mhvtl.conf at all",
			setup:     func(t *testing.T) (string, string) { t.Helper(); return t.TempDir(), "" },
			wantError: "read",
		},
		{
			name: "mhvtl.conf declares no media home",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(), "CAPACITY=500\n"), ""
			},
			wantError: "declares no MHVTL_HOME_PATH",
		},
		{
			name: "the declared media home does not exist",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(), "MHVTL_HOME_PATH="+filepath.Join(t.TempDir(), "absent")+"\n"), ""
			},
			wantError: "stat the mhvtl media home",
		},
		{
			name: "the declared media home is a file",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				file := filepath.Join(t.TempDir(), "not-a-dir")
				require.NoError(t, os.WriteFile(file, nil, 0o644))

				return writeConf(t, t.TempDir(), "MHVTL_HOME_PATH="+file+"\n"), ""
			},
			wantError: "is not a directory",
		},
		{
			name: "the declared media home needs shell expansion",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return writeConf(t, t.TempDir(), "MHVTL_HOME_PATH=$HOME/mhvtl\n"), ""
			},
			wantError: "does not expand",
		},
		{
			name: "an explicit media home that does not exist",
			setup: func(t *testing.T) (string, string) {
				t.Helper()

				return t.TempDir(), filepath.Join(t.TempDir(), "absent")
			},
			wantError: "stat the mhvtl media home",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configDir, homeDir := test.setup(t)

			opts := []Option{WithConfigDir(configDir)}
			if homeDir != "" {
				opts = append(opts, WithHomeDir(homeDir))
			}

			lib := newFakeLibrary(8, 3, 2)
			lib.slots[1] = "KEEP01L6"

			r := New(opts...)
			r.command = lib.command
			r.makeMedia = func(context.Context, string, string) error { return errors.New("must not run") }

			err := r.Mirror(t.Context(), lib, source(8, map[int]string{1: "X"}))

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantError)
			assert.Empty(t, lib.commands, "the library must not be touched")
			assert.Equal(t, "KEEP01L6", lib.slots[1])

			_, err = os.Stat(filepath.Join(configDir, "library_contents.10"))
			assert.True(t, os.IsNotExist(err), "library_contents must not have been rewritten")
		})
	}
}

func TestMirrorTakesTheMediaHomeFromMhvtlConf(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configDir := writeConf(t, t.TempDir(), "# mhvtl settings\nCAPACITY=500\nMHVTL_HOME_PATH="+homeDir+"\n")

	lib := newFakeLibrary(8, 3, 2)
	gotHome := ""

	r := New(WithConfigDir(configDir))
	r.command = lib.command
	r.makeMedia = func(_ context.Context, _, home string) error {
		gotHome = home
		materialise(t, home, "X")

		return nil
	}

	require.NoError(t, r.Mirror(t.Context(), lib, source(8, map[int]string{1: "X"})))
	assert.Equal(t, homeDir, gotHome)
}

func writeConf(t *testing.T, dir, body string) string {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, mhvtlConfFile), []byte(body), 0o644))

	return dir
}

func TestReadHomePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		conf string
	}{
		{name: "plain", conf: "MHVTL_HOME_PATH=/srv/mhvtl\n"},
		{name: "indented among other settings", conf: "CAPACITY=500\n  MHVTL_HOME_PATH=/srv/mhvtl\nVERBOSE=0\n"},
		{name: "quoted", conf: "MHVTL_HOME_PATH=\"/srv/mhvtl\"\n"},
		{name: "trailing comment", conf: "MHVTL_HOME_PATH=/srv/mhvtl # on the bulk pool\n"},
		{name: "commented-out assignment ignored", conf: "#MHVTL_HOME_PATH=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n"},
		{name: "last assignment wins", conf: "MHVTL_HOME_PATH=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n"},
		{name: "exported", conf: "export MHVTL_HOME_PATH=/srv/mhvtl\n"},
		{name: "similar name not matched", conf: "MHVTL_HOME_PATH_OLD=/opt/mhvtl\nMHVTL_HOME_PATH=/srv/mhvtl\n"},
		{name: "no trailing newline", conf: "MHVTL_HOME_PATH=/srv/mhvtl"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := readHomePath(filepath.Join(writeConf(t, t.TempDir(), test.conf), mhvtlConfFile))

			require.NoError(t, err)
			assert.Equal(t, "/srv/mhvtl", got)
		})
	}
}

func TestRenderLibraryContents(t *testing.T) {
	t.Parallel()

	got := RenderLibraryContents(tape.Inventory{Slots: []tape.StorageElement{
		{Address: 3, Full: true, Barcode: "REAL03L6"},
		{Address: 1, Full: true, Barcode: "REAL01L6"},
		{Address: 2},
		{Address: 4, Full: true}, // barcodeless: cannot be mirrored
	}})

	assert.Equal(t, "Drive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\nMAP 2:\nMAP 3:\n\n"+
		"Slot 1: REAL01L6\nSlot 2:\nSlot 3: REAL03L6\nSlot 4:\n", got)
}
