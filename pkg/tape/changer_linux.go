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

// SCSI opcodes issued to the changer.
const (
	opcodeModeSense6        = 0x1A // MODE SENSE(6)
	opcodeReadElementStatus = 0xB8 // READ ELEMENT STATUS
	opcodeMoveMedium        = 0xA5 // MOVE MEDIUM
)

const (
	// modeSenseDBD disables block descriptors.
	modeSenseDBD      = 0x08
	readElementVoltag = 0x10
	// readElementDVCID requests each drive's device identifier so Load pairs a
	// device node to its element by serial, not set position, and can never load
	// a blank into one drive while blank-checking another (#137). Libraries
	// without DVCID simply omit it.
	readElementDVCID       = 0x01
	readElementAllElements = 0xFFFF
	// elementStatusBufSize is far above any supported library's report;
	// decodeElementStatus still rejects a truncated response.
	elementStatusBufSize = 128 * 1024
	modeSenseBufSize     = 64
	senseBufferLen       = 32
)

// Changer command timeouts; MOVE MEDIUM is a physical robot move.
const (
	moveMediumTimeout  = 5 * time.Minute
	changerReadTimeout = 60 * time.Second
)

const senseKeyUnitAttention = 0x06

// A UNIT ATTENTION is queued per initiator after a state change (re-inventory,
// door access, bus reset) and reported on the next command, which is NOT
// performed — so retrying is safe even for MOVE MEDIUM. It stays queued however
// long the device sits idle, so a run's first command is the likeliest to draw
// one (#335).
const (
	unitAttentionAttempts   = 5
	unitAttentionRetryDelay = 500 * time.Millisecond
)

const scsiChangerClassDir = "/sys/class/scsi_changer"

// Inventory reads the library's element status via READ ELEMENT STATUS (0xB8),
// including the I/O-slot ACCESS bit the Eject phase uses to auto-resume.
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

// Load moves the tape in a 1-based storage slot into a 0-based drive.
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

// Unload moves the tape from a 0-based drive into a 1-based storage slot.
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

// Transfer moves media between two slots in mtx's combined storage+I/O
// numbering.
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

func (c *Changer) move(ctx context.Context, sgDevice string, addressing *elementAddressing, src, dst int) error {
	return withSGFile(sgDevice, func(f *os.File) error {
		return scsiMoveMedium(ctx, f, addressing.transportElement(), src, dst)
	})
}

// resolve lazily resolves and caches the sg node and element address map.
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

func (c *Changer) sgNode() (string, error) {
	if c.sgDevice != "" {
		return c.sgDevice, nil
	}

	if base := filepath.Base(c.device); strings.HasPrefix(base, "sg") {
		return c.device, nil
	}

	return sgDeviceForChangerNode(c.device)
}

// sgDeviceForChangerNode resolves the /dev/sgN sharing a /dev/schN's SCSI
// address.
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

// withSGFile runs fn on the sg node opened read/write; this needs
// CAP_SYS_RAWIO.
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

	if err := sgCommand(ctx, f, &hdr, cdb, sense, buf, "MODE SENSE(6)"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}

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

	if err := sgCommand(ctx, f, &hdr, cdb, sense, buf, "READ ELEMENT STATUS"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}

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

	return sgCommand(ctx, f, &hdr, cdb, sense, nil, fmt.Sprintf("MOVE MEDIUM (src=%d dst=%d)", src, dst))
}

// sgCommand runs one SG_IO command, retrying UNIT ATTENTION and turning any
// other failure into an error naming the sense. dxferBuf is kept alive for the
// call (nil for non-data commands).
func sgCommand(ctx context.Context, f *os.File, hdr *sgIOHdr, cdb, sense, dxferBuf []byte, name string) error {
	return retryUnitAttention(ctx, unitAttentionRetryDelay, func() (bool, error) {
		err := sgIoctl(f.Fd(), hdr)

		runtime.KeepAlive(cdb)
		runtime.KeepAlive(sense)
		runtime.KeepAlive(dxferBuf)

		if err != nil {
			return false, fmt.Errorf("SG_IO %s on %s: %w", name, f.Name(), err)
		}

		if hdr.status == 0 && hdr.hostStatus == 0 && hdr.driverStatus == 0 {
			return false, nil
		}

		key, asc, ascq, _ := parseSense(sense[:hdr.sbLenWr])

		return key == senseKeyUnitAttention, fmt.Errorf(
			"%s on %s failed (status=0x%02x host=0x%x driver=0x%x "+
				"sense_key=0x%02x asc=0x%02x ascq=0x%02x)",
			name, f.Name(), hdr.status, hdr.hostStatus, hdr.driverStatus, key, asc, ascq)
	})
}

// retryUnitAttention re-runs issue while it reports a UNIT ATTENTION, up to
// unitAttentionAttempts. The last error is returned unwrapped so a stuck target
// still surfaces its sense.
func retryUnitAttention(ctx context.Context, delay time.Duration, issue func() (unitAttention bool, err error)) error {
	var err error

	for attempt := 0; attempt < unitAttentionAttempts; attempt++ {
		var unitAttention bool

		unitAttention, err = issue()
		if err == nil || !unitAttention {
			return err
		}

		if attempt == unitAttentionAttempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w while retrying after a UNIT ATTENTION: %w", ctx.Err(), err)
		case <-time.After(delay):
		}
	}

	return err
}
