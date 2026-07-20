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

// errUnexpectedSense is returned when the blank-check READ(6) completes with a
// SCSI result that is neither "data present", "blank", nor "filemark" — i.e. a
// genuine drive/media error. IsBlank never reports such a tape as blank.
var errUnexpectedSense = errors.New("unexpected SCSI sense on blank-check read")

// SGDevice returns the SCSI generic device node (e.g. /dev/sg1) paired with
// this drive's tape node. When the node was not set explicitly with
// WithSGDevice it is resolved from the tape device's SCSI address. This is the
// node that LTFS and FormatTape use (the reference LTFS sg backend).
func (d *Drive) SGDevice() (string, error) {
	if d.sgDevice != "" {
		return d.sgDevice, nil
	}

	return sgDeviceForTapeNode(d.stDevice)
}

// IsBlank reports whether the loaded tape is blank (never written or fully
// erased). SPEC.md §4.3 step 6 uses this as the guard that a run never
// silently overwrites existing data ("Never write to a non-blank tape").
//
// Why this issues a raw SCSI READ(6) via SG_IO instead of read(2) on the st
// node:
//
//   - A blank tape answers a read at beginning-of-partition with a CHECK
//     CONDITION whose sense key is BLANK CHECK (0x08) / END-OF-DATA — exactly
//     how a real LTO drive reports blank media. The Linux st driver collapses
//     that into a bare EIO, indistinguishable from a genuine I/O error. For a
//     guard whose whole purpose is to refuse to overwrite a non-blank tape,
//     conflating "blank" with "read failure" is unacceptable: it could either
//     reject a perfectly blank tape or, worse, misclassify a faulty read.
//     Reading through SG_IO preserves the full sense data, so we can tell the
//     three outcomes apart precisely.
//
//   - It also minimises tape wear (SPEC.md §2, principle 2). We read a single
//     block at beginning-of-partition for any tape. The position-based
//     alternative (space-to-EOD, then check the block number) would wind a
//     *non-blank* tape all the way to its end-of-data — a full-cartridge
//     locate across exactly the recorded tape we are trying to protect.
//
// The decision is conservative: IsBlank returns (true, nil) only when the
// drive positively reports BLANK CHECK at the first block. Data, a filemark,
// or any unexpected sense all mean "not blank" or an error — never a false
// "blank".
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

	// Rewind to beginning-of-partition so the first block we read is the start
	// of the medium. On a real drive this can take a while, hence the generous
	// timeout; on a blank/near-BOP tape it returns immediately.
	if err = scsiRewind(ctx, f); err != nil {
		return false, err
	}

	res, err := scsiReadFirstBlock(ctx, f)
	if err != nil {
		return false, err
	}

	return interpretBlankProbe(res, f.Name())
}

// interpretBlankProbe turns a decoded blank-probe READ(6) into IsBlank's verdict.
// It is the pure decision half of IsBlank, split out so the decode logic is
// unit-testable without a real drive (status-only completions cannot be produced
// by mhvtl). It returns an error — never a false verdict — for any completion
// that does not positively resolve to blank, data, or filemark, so the caller's
// ready-retry loop retries a transient condition instead of recording a wrong
// answer.
func interpretBlankProbe(probe blankProbe, name string) (bool, error) {
	switch {
	case probe.status != statusGood && probe.status != statusCheckCondition:
		// A status-only completion (BUSY, RESERVATION CONFLICT, TASK SET FULL, ...)
		// carries no data phase and no sense: its residual and empty sense buffer
		// would otherwise be misread as transferred data (a false "not blank").
		// Report it as an error so blankCheckWhenReady retries rather than commit
		// to a definitive verdict from a completion that never read the medium.
		return false, fmt.Errorf(
			"read first block of %s (status=0x%02x): %w", name, probe.status, errUnexpectedSense)
	case probe.transferred > 0:
		// The first block returned data — the tape has recorded content.
		return false, nil
	case probe.senseKey == senseKeyBlankCheck:
		// BLANK CHECK at beginning-of-partition: end-of-data is the very first
		// block, so nothing has ever been written. This is the only blank case.
		return true, nil
	case probe.filemark:
		// A filemark is recorded structure, not blank media.
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

// sgDeviceForTapeNode resolves the SCSI generic node (/dev/sgN) that shares the
// same physical drive as the given non-rewinding tape node (/dev/nstN). Both
// classes expose a "device" symlink whose final component is the SCSI address
// (H:C:T:L); the sg node with the matching address is the same unit.
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

// sgDeviceForSCSIAddress returns the /dev/sgN node whose backing SCSI device is
// at the given address (H:C:T:L). The boolean reports whether a match was found.
// It is shared by the tape-drive and changer sg-node resolvers.
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

// scsiAddressOf returns the SCSI address (H:C:T:L) of a sysfs device node by
// reading its "device" symlink.
func scsiAddressOf(classDir, node string) (string, error) {
	link, err := os.Readlink(filepath.Join(classDir, node, "device"))
	if err != nil {
		return "", err
	}

	return filepath.Base(link), nil
}

// scsiAddressOfDevNode returns the SCSI address (H:C:T:L) of the device node at
// path, resolving it to its sysfs class entry by the node's device number (rdev)
// rather than assuming the path basename equals the kernel device name.
//
// The basename assumption holds for the raw library nodes (/dev/sch0 -> sch0,
// /dev/nst0 -> nst0) but not for the stable udev symlinks a dry run targets
// (/dev/mhvtl/changer -> sch2, /dev/mhvtl/drive0 -> nst0), whose basename is not
// a sysfs entry at all (issue #326). Matching on the device number is robust for
// the raw node, any symlink, and any kernel renaming, so it needs no special case.
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

// classEntryForDevNumber returns the name of the sysfs class entry under classDir
// whose "dev" file reports the given character-device number (major:minor). Each
// entry's "dev" file holds "major:minor\n"; the entry that matches names the same
// physical unit as the device node with that number, regardless of how that node
// is named or symlinked in /dev.
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

// --- SG_IO plumbing -------------------------------------------------------
//
// The structures and constants below mirror <scsi/sg.h> and the SCSI command
// set. They are deliberately self-contained (no cgo, no third-party deps).

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

	// firstBlockBufSize bounds the data transfer for the probe read. Any value
	// large enough to receive at least one byte of a recorded block suffices;
	// we only care whether *any* data comes back, not its full contents.
	firstBlockBufSize = 64 * 1024
)

// sgIOHdr mirrors struct sg_io_hdr (Linux, 64-bit). Field order and natural Go
// alignment reproduce the C layout the kernel expects (88 bytes).
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

// blankProbe holds the decoded result of the first-block READ(6).
type blankProbe struct {
	status      uint8
	transferred int
	senseKey    uint8
	asc         uint8
	ascq        uint8
	filemark    bool
}

// scsiRewind issues a SCSI REWIND and waits for completion.
func scsiRewind(ctx context.Context, f *os.File) error {
	// Heap-allocate the buffers so the addresses embedded in the SG_IO header
	// as uintptrs remain valid for the duration of the (blocking) ioctl.
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

// scsiReadFirstBlock issues a variable-length READ(6) (with SILI set) for the
// block at the current position and decodes the outcome.
func scsiReadFirstBlock(ctx context.Context, f *os.File) (blankProbe, error) {
	buf := make([]byte, firstBlockBufSize)
	sense := make([]byte, senseBufferLen)

	// READ(6): variable block (FIXED=0), SILI set so a short/over-length block
	// does not raise an incorrect-length CHECK CONDITION; the transfer length
	// is the buffer size encoded big-endian in bytes 2..4.
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

	// A non-zero host status, or a driver status beyond DRIVER_SENSE, is a
	// transport-level failure rather than a SCSI sense condition; surface it as
	// an error rather than guessing blankness. A plain CHECK CONDITION sets
	// DRIVER_SENSE and is expected here (blank check / incorrect length).
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

// parseSense decodes the sense key, ASC, ASCQ, and filemark flag from a SCSI
// sense buffer in either fixed (0x70/0x71) or descriptor (0x72/0x73) format.
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
		// The filemark bit lives in a stream-commands sense data descriptor,
		// not in the fixed header; we do not need it for the blank decision
		// (which keys on the sense key), so leave filemark false here.
		if len(sb) > 3 {
			key = sb[1] & 0x0f
			asc = sb[2]
			ascq = sb[3]
		}
	}

	return key, asc, ascq, filemark
}

// sgIoctl invokes the SG_IO ioctl with the given header.
func sgIoctl(fd uintptr, hdr *sgIOHdr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(sgIO), uintptr(unsafe.Pointer(hdr)))
	if errno != 0 {
		return errno
	}

	return nil
}

// Default command timeouts. Rewind on a full cartridge can take several minutes
// on real hardware; a blank-check read is near-instant.
const (
	rewindTimeout = 5 * time.Minute
	readTimeout   = 60 * time.Second
)

// timeoutMs returns the SG_IO timeout in milliseconds: the context's remaining
// time if it has a deadline sooner than def, otherwise def. SG_IO is a blocking
// ioctl, so this timeout — not context cancellation — bounds how long a command
// may hang. The returned value is the kernel's required unit (sg_io_hdr.timeout
// is a uint32 of milliseconds); def is always small enough that the conversion
// cannot overflow.
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
