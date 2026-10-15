//go:build linux

package tape

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errUnexpectedSense marks a blank-check READ(6) result that is neither data,
// blank, nor filemark; such a tape is never reported blank.
var errUnexpectedSense = errors.New("unexpected SCSI sense on blank-check read")

// SGDevice returns the SCSI generic node paired with this drive's tape node,
// which LTFS and FormatTape use.
func (d *Drive) SGDevice() (string, error) {
	if d.sgDevice != "" {
		return d.sgDevice, nil
	}

	return sgDeviceForTapeNode(d.stDevice)
}

// IsBlank reports whether the loaded tape is blank; it is the guard that a run
// never overwrites existing data (SPEC.md §4.3). It returns true only when the
// drive positively reports BLANK CHECK at the first block.
//
// It uses a raw SG_IO READ(6) because the st driver collapses BLANK CHECK into
// a bare EIO, indistinguishable from a real I/O error. Reading one block at BOP
// also avoids space-to-EOD, which would wind a non-blank tape end to end.
func (d *Drive) IsBlank(ctx context.Context) (blank bool, err error) {
	sgDevice := d.sgDevice
	if sgDevice == "" {
		sgDevice, err = sgDeviceForTapeNode(d.stDevice)
		if err != nil {
			return false, err
		}
	}

	f, err := os.OpenFile(sgDevice, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("open sg device %s: %w", sgDevice, err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close sg device %s: %w", sgDevice, cerr)
		}
	}()

	if err = scsiRewind(ctx, f); err != nil {
		return false, err
	}

	res, err := scsiReadFirstBlock(ctx, f)
	if err != nil {
		return false, err
	}

	return interpretBlankProbe(res, f.Name())
}

// interpretBlankProbe turns a decoded blank-probe READ(6) into IsBlank's
// verdict. Anything not positively blank, data, or filemark is an error so the
// caller retries rather than recording a wrong answer.
func interpretBlankProbe(probe blankProbe, name string) (bool, error) {
	switch {
	case probe.status != statusGood && probe.status != statusCheckCondition:
		// A status-only completion (BUSY, RESERVATION CONFLICT, ...) never read
		// the medium; its residual would otherwise look like transferred data.
		return false, fmt.Errorf(
			"read first block of %s (status=0x%02x): %w", name, probe.status, errUnexpectedSense)
	case probe.transferred > 0:
		return false, nil
	case probe.senseKey == senseKeyBlankCheck:
		return true, nil
	case probe.filemark:
		return false, nil
	default:
		return false, fmt.Errorf(
			"read first block of %s (status=0x%02x sense_key=0x%02x asc=0x%02x ascq=0x%02x): %w",
			name, probe.status, probe.senseKey, probe.asc, probe.ascq, errUnexpectedSense)
	}
}

// --- SCSI generic node resolution ----------------------------------------

const (
	scsiTapeClassDir    = "/sys/class/scsi_tape"
	scsiGenericClassDir = "/sys/class/scsi_generic"
)

// sgDeviceForTapeNode resolves the /dev/sgN sharing a /dev/nstN's SCSI address.
func sgDeviceForTapeNode(device string) (string, error) {
	tapeAddr, err := scsiAddressOfDevNode(scsiTapeClassDir, device)
	if err != nil {
		return "", fmt.Errorf("resolve SCSI address of tape device %s: %w", device, err)
	}

	if sgDevice, ok := sgDeviceForSCSIAddress(tapeAddr); ok {
		return sgDevice, nil
	}

	return "", fmt.Errorf("no SCSI generic node found for tape device %s (SCSI address %s); "+
		"set it explicitly with WithSGDevice", device, tapeAddr)
}

// sgDeviceForSCSIAddress returns the /dev/sgN at SCSI address H:C:T:L.
func sgDeviceForSCSIAddress(scsiAddr string) (string, bool) {
	entries, err := os.ReadDir(scsiGenericClassDir)
	if err != nil {
		return "", false
	}

	for _, entry := range entries {
		addr, err := scsiAddressOf(scsiGenericClassDir, entry.Name())
		if err != nil {
			continue
		}

		if addr == scsiAddr {
			return filepath.Join("/dev", entry.Name()), true
		}
	}

	return "", false
}

// scsiAddressOf reads a sysfs class entry's H:C:T:L from its "device" symlink.
func scsiAddressOf(classDir, node string) (string, error) {
	link, err := os.Readlink(filepath.Join(classDir, node, "device"))
	if err != nil {
		return "", err
	}

	return filepath.Base(link), nil
}

// scsiAddressOfDevNode returns the H:C:T:L of the device node at path, matching
// its sysfs entry by device number: udev symlinks such as /dev/mhvtl/changer
// have basenames that are not sysfs entries (#326).
func scsiAddressOfDevNode(classDir, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("stat %s: no syscall.Stat_t (got %T)", path, info.Sys())
	}

	rdev := uint64(stat.Rdev)

	name, err := classEntryForDevNumber(classDir, unix.Major(rdev), unix.Minor(rdev))
	if err != nil {
		return "", err
	}

	return scsiAddressOf(classDir, name)
}

// classEntryForDevNumber returns the classDir entry whose "dev" file is
// major:minor.
func classEntryForDevNumber(classDir string, major, minor uint32) (string, error) {
	want := fmt.Sprintf("%d:%d", major, minor)

	entries, err := os.ReadDir(classDir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(classDir, entry.Name(), "dev"))
		if err != nil {
			continue
		}

		if strings.TrimSpace(string(data)) == want {
			return entry.Name(), nil
		}
	}

	return "", fmt.Errorf("no %s entry has device number %s", classDir, want)
}

// --- SG_IO plumbing (mirrors <scsi/sg.h>; no cgo) ---------------------------

const (
	sgIO = 0x2285 // SG_IO ioctl request

	sgDxferNone    = -1 // SG_DXFER_NONE
	sgDxferFromDev = -3 // SG_DXFER_FROM_DEV

	driverSense = 0x08 // DRIVER_SENSE bit in sg_io_hdr.driver_status

	statusGood           = 0x00 // SCSI status GOOD (data phase completed)
	statusCheckCondition = 0x02 // SCSI status CHECK CONDITION (sense data present)

	opcodeRewind = 0x01 // REWIND
	opcodeRead6  = 0x08 // READ(6)
	read6SILI    = 0x02 // SILI bit in READ(6) byte 1 (suppress incorrect-length)

	senseKeyBlankCheck = 0x08 // SCSI sense key BLANK CHECK
	senseFilemarkBit   = 0x80 // FILEMARK bit in fixed-format sense byte 2

	// firstBlockBufSize: the probe only needs to see whether any data comes back.
	firstBlockBufSize = 64 * 1024
)

// sgIOHdr mirrors struct sg_io_hdr (Linux, 64-bit, 88 bytes); field order is
// load-bearing.
type sgIOHdr struct {
	interfaceID    int32   // 'S'
	dxferDirection int32   // SG_DXFER_*
	cmdLen         uint8   // CDB length
	mxSbLen        uint8   // sense-buffer capacity
	iovecCount     uint16  // 0 (no scatter/gather)
	dxferLen       uint32  // data-transfer length
	dxferp         uintptr // data buffer
	cmdp           uintptr // CDB
	sbp            uintptr // sense buffer
	timeout        uint32  // milliseconds
	flags          uint32  // 0
	packID         int32   // 0
	usrPtr         uintptr // 0
	status         uint8   // SCSI status byte
	maskedStatus   uint8   //
	msgStatus      uint8   //
	sbLenWr        uint8   // sense bytes actually written
	hostStatus     uint16  //
	driverStatus   uint16  //
	resid          int32   // dxferLen - bytes actually transferred
	duration       uint32  //
	info           uint32  //
}

type blankProbe struct {
	status      uint8
	transferred int
	senseKey    uint8
	asc         uint8
	ascq        uint8
	filemark    bool
}

func scsiRewind(ctx context.Context, f *os.File) error {
	// Heap buffers, kept alive below, since the header holds them as uintptrs.
	cdb := []byte{opcodeRewind, 0, 0, 0, 0, 0}
	sense := make([]byte, senseBufferLen)

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferNone,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, rewindTimeout),
	}

	err := sgIoctl(f.Fd(), &hdr)

	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)

	if err != nil {
		return fmt.Errorf("SG_IO REWIND on %s: %w", f.Name(), err)
	}

	if hdr.status != 0 || hdr.hostStatus != 0 || hdr.driverStatus != 0 {
		key, asc, ascq, _ := parseSense(sense[:hdr.sbLenWr])

		return fmt.Errorf("REWIND on %s failed (status=0x%02x host=0x%x driver=0x%x "+
			"sense_key=0x%02x asc=0x%02x ascq=0x%02x)",
			f.Name(), hdr.status, hdr.hostStatus, hdr.driverStatus, key, asc, ascq)
	}

	return nil
}

// scsiReadFirstBlock issues a variable-length READ(6) at the current position.
func scsiReadFirstBlock(ctx context.Context, f *os.File) (blankProbe, error) {
	buf := make([]byte, firstBlockBufSize)
	sense := make([]byte, senseBufferLen)

	// SILI suppresses the incorrect-length CHECK CONDITION for a short block.
	n := uint32(len(buf))
	cdb := []byte{
		opcodeRead6,
		read6SILI,
		byte(n >> 16),
		byte(n >> 8),
		byte(n),
		0,
	}

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferFromDev,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		dxferLen:       n,
		dxferp:         uintptr(unsafe.Pointer(&buf[0])),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, readTimeout),
	}

	err := sgIoctl(f.Fd(), &hdr)

	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	runtime.KeepAlive(buf)

	if err != nil {
		return blankProbe{}, fmt.Errorf("SG_IO READ(6) on %s: %w", f.Name(), err)
	}

	// Anything beyond DRIVER_SENSE (which a CHECK CONDITION sets) is a transport
	// failure; never guess blankness from it.
	if hdr.hostStatus != 0 || hdr.driverStatus&^driverSense != 0 {
		return blankProbe{}, fmt.Errorf("READ(6) on %s transport error "+
			"(host=0x%x driver=0x%x)", f.Name(), hdr.hostStatus, hdr.driverStatus)
	}

	key, asc, ascq, filemark := parseSense(sense[:hdr.sbLenWr])

	return blankProbe{
		status:      hdr.status,
		transferred: int(hdr.dxferLen) - int(hdr.resid),
		senseKey:    key,
		asc:         asc,
		ascq:        ascq,
		filemark:    filemark,
	}, nil
}

// parseSense decodes fixed- or descriptor-format sense data.
func parseSense(sb []byte) (key, asc, ascq byte, filemark bool) {
	if len(sb) == 0 {
		return 0, 0, 0, false
	}

	switch sb[0] & 0x7f {
	case 0x70, 0x71: // fixed-format sense
		if len(sb) > 2 {
			key = sb[2] & 0x0f
			filemark = sb[2]&senseFilemarkBit != 0
		}

		if len(sb) > 13 {
			asc = sb[12]
			ascq = sb[13]
		}
	case 0x72, 0x73: // descriptor-format sense
		// The filemark bit lives in a separate descriptor; the blank decision
		// does not need it.
		if len(sb) > 3 {
			key = sb[1] & 0x0f
			asc = sb[2]
			ascq = sb[3]
		}
	}

	return key, asc, ascq, filemark
}

func sgIoctl(fd uintptr, hdr *sgIOHdr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(sgIO), uintptr(unsafe.Pointer(hdr)))
	if errno != 0 {
		return errno
	}

	return nil
}

// Rewinding a full cartridge can take minutes.
const (
	rewindTimeout = 5 * time.Minute
	readTimeout   = 60 * time.Second
)

// timeoutMs returns min(def, ctx's remaining time) in ms. SG_IO is blocking, so
// this timeout, not ctx cancellation, bounds a hung command.
func timeoutMs(ctx context.Context, def time.Duration) uint32 {
	timeout := def

	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	ms := timeout.Milliseconds()
	if ms <= 0 {
		return 1
	}

	return uint32(ms)
}

func testUnitReady(ctx context.Context, sgDevice string) error {
	return withSGFile(sgDevice, func(f *os.File) error {
		cdb := []byte{0x00, 0, 0, 0, 0, 0}
		sense := make([]byte, senseBufferLen)

		hdr := sgIOHdr{
			interfaceID:    'S',
			dxferDirection: sgDxferNone,
			cmdLen:         uint8(len(cdb)),
			mxSbLen:        uint8(len(sense)),
			cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
			sbp:            uintptr(unsafe.Pointer(&sense[0])),
			timeout:        timeoutMs(ctx, 30*time.Second),
		}

		return sgCommand(ctx, f, &hdr, cdb, sense, nil, "TEST UNIT READY")
	})
}
