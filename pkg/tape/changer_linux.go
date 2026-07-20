//go:build linux

package tape

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"
)

// This file issues the changer's SCSI commands directly via SG_IO, reusing the
// cgo-free plumbing in drive_linux.go (sgIOHdr, sgIoctl, parseSense,
// scsiAddressOf, timeoutMs, and the dxfer/driver-status constants). It replaces
// the former mtx subprocess; the binary decoding it feeds lives in
// changer_decode.go.

// SCSI opcodes issued to the changer.
const (
	opcodeModeSense6        = 0x1A // MODE SENSE(6)
	opcodeReadElementStatus = 0xB8 // READ ELEMENT STATUS
	opcodeMoveMedium        = 0xA5 // MOVE MEDIUM
)

const (
	// modeSenseDBD disables block descriptors so the mode page follows the
	// 4-byte header directly.
	modeSenseDBD = 0x08
	// readElementVoltag sets VOLTAG in the READ ELEMENT STATUS CDB so each
	// descriptor carries its primary volume tag (element type code 0 = all
	// types).
	readElementVoltag = 0x10
	// readElementDVCID sets DVCID (byte 6, bit 0) in the READ ELEMENT STATUS CDB
	// so each data-transfer element descriptor carries the drive's primary device
	// identifier (its unit serial). The Load phase pairs a configured drive device
	// node to its changer element by that identity — not by set position — so a
	// probe-order or retry-set mismatch can never load a blank into one physical
	// drive while blank-checking another (issue #137). Libraries that do not
	// implement DVCID simply omit the identifier; the decode tolerates its absence.
	readElementDVCID = 0x01
	// readElementAllElements is the "number of elements" CDB field value that
	// requests every element the library has.
	readElementAllElements = 0xFFFF
	// elementStatusBufSize bounds the READ ELEMENT STATUS transfer. It is far
	// larger than any supported library's report (the production 3573-TL and the
	// mhvtl L700 are well under 4 KiB), so a truncated response is not expected.
	// decodeElementStatus enforces this rather than assuming it: it compares the
	// report's declared byte count against the bytes transferred and rejects any
	// response that would have overrun this buffer.
	elementStatusBufSize = 128 * 1024
	// modeSenseBufSize bounds the MODE SENSE transfer; the 0x1D page plus header
	// is 24 bytes.
	modeSenseBufSize = 64
	// senseBufferLen sizes the SCSI sense buffer for every changer command; a
	// fixed-format sense response fits well within it.
	senseBufferLen = 32
)

// Changer command timeouts. A MOVE MEDIUM is a physical robot move and can take
// a while on real hardware; the status reads are near-instant.
const (
	moveMediumTimeout  = 5 * time.Minute
	changerReadTimeout = 60 * time.Second
)

// scsiChangerClassDir is the sysfs class directory for SCSI media changers,
// paralleling scsiTapeClassDir/scsiGenericClassDir in drive_linux.go.
const scsiChangerClassDir = "/sys/class/scsi_changer"

// Inventory reads the library's element status via READ ELEMENT STATUS (0xB8)
// and returns the decoded topology with friendly drive/slot numbers. The
// import/export ACCESS bit is reported for every I/O slot (Inventory.IOAccessReported),
// which the operator-in-the-loop Eject phase uses to auto-resume (SPEC §4.3 phase 8).
func (c *Changer) Inventory(ctx context.Context) (Inventory, error) {
	sgDevice, addressing, err := c.resolve(ctx)
	if err != nil {
		return Inventory{}, err
	}

	var inv Inventory

	err = withSGFile(sgDevice, func(f *os.File) error {
		data, err := scsiReadElementStatus(ctx, f)
		if err != nil {
			return err
		}

		inv, err = decodeElementStatus(data, addressing)

		return err
	})
	if err != nil {
		return Inventory{}, err
	}

	return inv, nil
}

// Load moves the tape in the given storage slot into the given drive (both
// friendly numbers: a 1-based storage slot and a 0-based drive index).
func (c *Changer) Load(ctx context.Context, slot, drive int) error {
	sgDevice, addressing, err := c.resolve(ctx)
	if err != nil {
		return err
	}

	src, err := addressing.rawStorage(slot)
	if err != nil {
		return err
	}

	dst, err := addressing.rawDrive(drive)
	if err != nil {
		return err
	}

	return c.move(ctx, sgDevice, addressing, src, dst)
}

// Unload moves the tape from the given drive (0-based index) into the given
// storage slot (1-based).
func (c *Changer) Unload(ctx context.Context, slot, drive int) error {
	sgDevice, addressing, err := c.resolve(ctx)
	if err != nil {
		return err
	}

	src, err := addressing.rawDrive(drive)
	if err != nil {
		return err
	}

	dst, err := addressing.rawStorage(slot)
	if err != nil {
		return err
	}

	return c.move(ctx, sgDevice, addressing, src, dst)
}

// Transfer moves media from srcSlot to dstSlot. Both are friendly slot numbers in
// mtx's combined storage+I/O numbering (storage slots first, then I/O-station
// slots), so this moves a tape between a storage slot and an I/O station slot in
// either direction.
func (c *Changer) Transfer(ctx context.Context, srcSlot, dstSlot int) error {
	sgDevice, addressing, err := c.resolve(ctx)
	if err != nil {
		return err
	}

	src, err := addressing.rawSlot(srcSlot)
	if err != nil {
		return err
	}

	dst, err := addressing.rawSlot(dstSlot)
	if err != nil {
		return err
	}

	return c.move(ctx, sgDevice, addressing, src, dst)
}

// move issues a MOVE MEDIUM from raw source to raw destination using the
// library's transport element as the robot.
func (c *Changer) move(ctx context.Context, sgDevice string, addressing *elementAddressing, src, dst int) error {
	return withSGFile(sgDevice, func(f *os.File) error {
		return scsiMoveMedium(ctx, f, addressing.transportElement(), src, dst)
	})
}

// resolve lazily resolves and caches the SCSI generic node and the element
// address map (mode page 0x1D), so repeated calls on the same Changer issue the
// mode sense only once.
func (c *Changer) resolve(ctx context.Context) (string, *elementAddressing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.resolvedSG == "" {
		sgDevice, err := c.sgNode()
		if err != nil {
			return "", nil, err
		}

		c.resolvedSG = sgDevice
	}

	if c.addressing == nil {
		addressing, err := readElementAddressing(ctx, c.resolvedSG)
		if err != nil {
			return "", nil, err
		}

		c.addressing = addressing
	}

	return c.resolvedSG, c.addressing, nil
}

// sgNode returns the SCSI generic node to issue commands to: the explicit
// override if set, the device itself when it is already an sg node, otherwise the
// node resolved from the changer device's SCSI address.
func (c *Changer) sgNode() (string, error) {
	if c.sgDevice != "" {
		return c.sgDevice, nil
	}

	if base := filepath.Base(c.device); strings.HasPrefix(base, "sg") {
		return c.device, nil
	}

	return sgDeviceForChangerNode(c.device)
}

// sgDeviceForChangerNode resolves the SCSI generic node (/dev/sgN) that shares
// the same physical unit as the given changer node (/dev/schN), matching on the
// SCSI address (H:C:T:L). It mirrors sgDeviceForTapeNode.
func sgDeviceForChangerNode(device string) (string, error) {
	changerAddr, err := scsiAddressOfDevNode(scsiChangerClassDir, device)
	if err != nil {
		return "", fmt.Errorf("resolve SCSI address of changer device %s: %w", device, err)
	}

	if sgDevice, ok := sgDeviceForSCSIAddress(changerAddr); ok {
		return sgDevice, nil
	}

	return "", fmt.Errorf("no SCSI generic node found for changer device %s (SCSI address %s); "+
		"set it explicitly with WithChangerSGDevice", device, changerAddr)
}

// readElementAddressing issues MODE SENSE(6) for the Element Address Assignment
// page (0x1D) and decodes the raw↔friendly address map.
func readElementAddressing(ctx context.Context, sgDevice string) (*elementAddressing, error) {
	var addressing *elementAddressing

	err := withSGFile(sgDevice, func(f *os.File) error {
		data, err := scsiModeSense6(ctx, f, modePageElementAddr)
		if err != nil {
			return err
		}

		addressing, err = parseElementAddressing(data)

		return err
	})
	if err != nil {
		return nil, err
	}

	return addressing, nil
}

// withSGFile opens the SCSI generic node read/write, runs fn, and closes it,
// surfacing a close error only when fn itself succeeded. Issuing SCSI commands
// requires CAP_SYS_RAWIO plus device access — the same privilege the blank check
// and the former mtx invocation needed.
func withSGFile(device string, fn func(*os.File) error) (err error) {
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open changer device %s: %w", device, err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close changer device %s: %w", device, cerr)
		}
	}()

	return fn(f)
}

// scsiModeSense6 issues MODE SENSE(6) for the given page (block descriptors
// disabled) and returns the raw response bytes.
func scsiModeSense6(ctx context.Context, f *os.File, page byte) ([]byte, error) {
	buf := make([]byte, modeSenseBufSize)
	sense := make([]byte, senseBufferLen)

	cdb := []byte{opcodeModeSense6, modeSenseDBD, page, 0, byte(len(buf)), 0}

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferFromDev,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		dxferLen:       uint32(len(buf)),
		dxferp:         uintptr(unsafe.Pointer(&buf[0])),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, changerReadTimeout),
	}

	if err := sgCommand(f, &hdr, cdb, sense, buf, "MODE SENSE(6)"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}

// scsiReadElementStatus issues READ ELEMENT STATUS (0xB8) for all element types
// with volume tags and returns the raw response bytes.
func scsiReadElementStatus(ctx context.Context, f *os.File) ([]byte, error) {
	buf := make([]byte, elementStatusBufSize)
	sense := make([]byte, senseBufferLen)

	alloc := uint32(len(buf))
	cdb := []byte{
		opcodeReadElementStatus,
		readElementVoltag, // VOLTAG set, element type code 0 (all types)
		0, 0,              // starting element address 0
		byte(readElementAllElements >> 8), byte(readElementAllElements & 0xFF), // number of elements
		readElementDVCID,                                 // DVCID set (CURDATA off): report per-drive device identifiers
		byte(alloc >> 16), byte(alloc >> 8), byte(alloc), // allocation length (24-bit)
		0, // reserved
		0, // control
	}

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferFromDev,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		dxferLen:       alloc,
		dxferp:         uintptr(unsafe.Pointer(&buf[0])),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, changerReadTimeout),
	}

	if err := sgCommand(f, &hdr, cdb, sense, buf, "READ ELEMENT STATUS"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}

// scsiMoveMedium issues MOVE MEDIUM (0xA5) to move media from src to dst using
// the given transport element (robot) as the origin of the arm.
func scsiMoveMedium(ctx context.Context, f *os.File, transport, src, dst int) error {
	sense := make([]byte, senseBufferLen)

	cdb := []byte{
		opcodeMoveMedium,
		0,
		byte(transport >> 8), byte(transport),
		byte(src >> 8), byte(src),
		byte(dst >> 8), byte(dst),
		0, 0,
		0, // INVERT off
		0, // control
	}

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferNone,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, moveMediumTimeout),
	}

	return sgCommand(f, &hdr, cdb, sense, nil, fmt.Sprintf("MOVE MEDIUM (src=%d dst=%d)", src, dst))
}

// sgCommand runs one SG_IO command and turns a transport failure or CHECK
// CONDITION into a descriptive error naming the SCSI sense. dxferBuf is the data
// buffer to keep alive (nil for non-data commands). It is the package's shared
// SG_IO runner, used by the changer commands here and by INQUIRY (inquiry_linux.go).
func sgCommand(f *os.File, hdr *sgIOHdr, cdb, sense, dxferBuf []byte, name string) error {
	err := sgIoctl(f.Fd(), hdr)

	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	runtime.KeepAlive(dxferBuf)

	if err != nil {
		return fmt.Errorf("SG_IO %s on %s: %w", name, f.Name(), err)
	}

	if hdr.status != 0 || hdr.hostStatus != 0 || hdr.driverStatus != 0 {
		key, asc, ascq, _ := parseSense(sense[:hdr.sbLenWr])

		return fmt.Errorf("%s on %s failed (status=0x%02x host=0x%x driver=0x%x "+
			"sense_key=0x%02x asc=0x%02x ascq=0x%02x)",
			name, f.Name(), hdr.status, hdr.hostStatus, hdr.driverStatus, key, asc, ascq)
	}

	return nil
}
