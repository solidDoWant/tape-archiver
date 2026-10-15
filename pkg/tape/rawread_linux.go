//go:build linux

package tape

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// RawReader reads a loaded tape's raw blocks by SCSI address without an LTFS
// mount, for index-loss recovery from a captured index (SPEC §10, #21). It
// satisfies ltfs.BlockReader and is not safe for concurrent use.
type RawReader struct {
	file    *os.File
	bufSize int
}

// OpenRawReader opens a RawReader on this drive's sg node; the tape must
// already be loaded.
func (d *Drive) OpenRawReader() (*RawReader, error) {
	sgDevice, err := d.SGDevice()
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(sgDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open sg device %s: %w", sgDevice, err)
	}

	// The sg default reserved buffer (32 KiB) is far below an LTFS block, like
	// real LTFS we grow it. Best-effort: the indirect path also works.
	_, _ = growReservedSize(file, rawReadBufSize)

	return &RawReader{file: file, bufSize: rawReadBufSize}, nil
}

// Close releases the sg device.
func (r *RawReader) Close() error {
	return r.file.Close()
}

// Locate positions the tape at (partition, block); partition is the LTFS
// label "a" or "b".
func (r *RawReader) Locate(ctx context.Context, partition string, block uint64) error {
	number, err := partitionNumber(partition)
	if err != nil {
		return err
	}

	return scsiLocate(ctx, r.file, number, block)
}

// ReadBlock reads the block at the current position and advances one block.
func (r *RawReader) ReadBlock(ctx context.Context) ([]byte, error) {
	return scsiReadBlock(ctx, r.file, r.bufSize)
}

const (
	opcodeLocate16 = 0x92 // LOCATE(16)
	locate16CP     = 0x02 // CP (change partition) bit in LOCATE(16) byte 1

	sgSetReservedSize = 0x2275 // SG_SET_RESERVED_SIZE ioctl
	sgGetReservedSize = 0x2272 // SG_GET_RESERVED_SIZE ioctl

	senseKeyNoSense = 0x00 // SCSI sense key NO SENSE (informational conditions)

	// rawReadBufSize is the LTFS default block size and the largest READ the sg
	// path accepts (1 MiB fails with EINVAL), so a full buffer is a complete
	// block, not a truncated one.
	rawReadBufSize = 512 * 1024

	// mkltfs default partition numbers for LTFS labels "a" and "b".
	ltfsIndexPartition = 0 // "a"
	ltfsDataPartition  = 1 // "b"
)

const locateTimeout = 5 * time.Minute

// growReservedSize grows the sg reserved buffer and returns the size the kernel
// granted.
func growReservedSize(f *os.File, want int) (int, error) {
	size := int32(want)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(sgSetReservedSize), uintptr(unsafe.Pointer(&size))); errno != 0 {
		return 0, fmt.Errorf("SG_SET_RESERVED_SIZE: %w", errno)
	}

	effective := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(sgGetReservedSize), uintptr(unsafe.Pointer(&effective))); errno != 0 {
		return 0, fmt.Errorf("SG_GET_RESERVED_SIZE: %w", errno)
	}

	if effective <= 0 {
		return 0, fmt.Errorf("sg driver granted a non-positive reserved size %d", effective)
	}

	return int(effective), nil
}

func partitionNumber(label string) (uint8, error) {
	switch label {
	case "a", "A":
		return ltfsIndexPartition, nil
	case "b", "B":
		return ltfsDataPartition, nil
	default:
		return 0, fmt.Errorf("unknown LTFS partition label %q (want \"a\" or \"b\")", label)
	}
}

// scsiLocate issues LOCATE(16) with CP set (DEST_TYPE 0: logical block).
func scsiLocate(ctx context.Context, f *os.File, partition uint8, block uint64) error {
	cdb := make([]byte, 16)
	cdb[0] = opcodeLocate16
	cdb[1] = locate16CP
	cdb[3] = partition
	binary.BigEndian.PutUint64(cdb[4:12], block)

	sense := make([]byte, senseBufferLen)

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferNone,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, locateTimeout),
	}

	err := sgIoctl(f.Fd(), &hdr)

	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)

	if err != nil {
		return fmt.Errorf("SG_IO LOCATE(16) on %s: %w", f.Name(), err)
	}

	if hdr.status != 0 || hdr.hostStatus != 0 || hdr.driverStatus != 0 {
		key, asc, ascq, _ := parseSense(sense[:hdr.sbLenWr])

		return fmt.Errorf("LOCATE(16) to partition %d block %d on %s failed "+
			"(status=0x%02x host=0x%x driver=0x%x sense_key=0x%02x asc=0x%02x ascq=0x%02x)",
			partition, block, f.Name(), hdr.status, hdr.hostStatus, hdr.driverStatus, key, asc, ascq)
	}

	return nil
}

// scsiReadBlock issues a variable-length READ(6) at the current position.
func scsiReadBlock(ctx context.Context, f *os.File, bufSize int) ([]byte, error) {
	buf := make([]byte, bufSize)
	sense := make([]byte, senseBufferLen)

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
		return nil, fmt.Errorf("SG_IO READ(6) on %s: %w", f.Name(), err)
	}

	return decodeReadBlock(&hdr, sense[:hdr.sbLenWr], buf, f.Name())
}

// decodeReadBlock returns a copy of the block payload, or an error when the
// position holds no readable data block (a mis-locate, never silent truncation).
func decodeReadBlock(hdr *sgIOHdr, sense, buf []byte, name string) ([]byte, error) {
	if hdr.hostStatus != 0 || hdr.driverStatus&^driverSense != 0 {
		return nil, fmt.Errorf("READ(6) on %s transport error (host=0x%x driver=0x%x)",
			name, hdr.hostStatus, hdr.driverStatus)
	}

	// Status-only completions (BUSY, ...) had no data phase; their residual would
	// otherwise be misread as a full transfer.
	if hdr.status != statusGood && hdr.status != statusCheckCondition {
		return nil, fmt.Errorf("READ(6) on %s returned no data block "+
			"(status=0x%02x)", name, hdr.status)
	}

	key, asc, ascq, filemark := parseSense(sense)
	transferred := int(hdr.dxferLen) - int(hdr.resid)

	// NO SENSE is the incorrect-length indication for a short LTFS block, which
	// still carries data. Any real sense key or a filemark is a mis-locate.
	if filemark || key != senseKeyNoSense {
		return nil, fmt.Errorf("READ(6) on %s returned no data block "+
			"(status=0x%02x sense_key=0x%02x asc=0x%02x ascq=0x%02x filemark=%t)",
			name, hdr.status, key, asc, ascq, filemark)
	}

	if transferred <= 0 {
		return nil, fmt.Errorf("READ(6) on %s returned 0 bytes", name)
	}

	out := make([]byte, transferred)
	copy(out, buf[:transferred])

	return out, nil
}
