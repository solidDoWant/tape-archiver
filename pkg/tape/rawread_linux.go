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

// RawReader reads a loaded tape's raw physical blocks by SCSI address, with no
// LTFS mount and no reliance on the on-tape index. It is the load-bearing
// primitive for index-loss recovery (SPEC §10; issue #21): paired with a
// captured LTFS index, it lets a recoverer SCSI-LOCATE to an extent's
// (partition, block) and read the bytes straight back, even when the on-tape
// LTFS index is unreadable and LTFS will not mount.
//
// Its Locate/ReadBlock method set satisfies the ltfs.BlockReader seam the extent
// extractor drives, without this package importing pkg/ltfs (the interface is
// primitive-typed), so there is no dependency cycle.
//
// A RawReader holds its sg device open across calls; Close it when done. It
// keeps a position, so it is not safe for concurrent use.
type RawReader struct {
	file    *os.File
	bufSize int
}

// OpenRawReader opens a RawReader on this drive's SCSI generic node (the same
// node LTFS and the blank check use). The tape must already be loaded. Close the
// returned reader to release the device.
//
// It grows the sg per-file reserved transfer buffer to hold a full LTFS block:
// the driver's default reserved size (32 KiB) is far smaller than the LTFS block
// (512 KiB by default), and a READ whose transfer length exceeds what the driver
// can buffer fails with EINVAL — as real LTFS does, we raise the reserved size
// first. The size the kernel grants bounds a single block read.
func (d *Drive) OpenRawReader() (*RawReader, error) {
	sgDevice, err := d.SGDevice()
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(sgDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open sg device %s: %w", sgDevice, err)
	}

	// Best-effort: grow the sg reserved buffer toward a full block. The transfer
	// also works via the driver's indirect path when the reserved buffer stays
	// small, so a failure here is not fatal — the read is bounded by rawReadBufSize
	// (the max single transfer the sg path accepts) regardless.
	_, _ = growReservedSize(file, rawReadBufSize)

	return &RawReader{file: file, bufSize: rawReadBufSize}, nil
}

// Close releases the underlying sg device.
func (r *RawReader) Close() error {
	return r.file.Close()
}

// Locate positions the tape so the next ReadBlock returns the block at
// (partition, block). partition is the LTFS partition label ("a" or "b").
func (r *RawReader) Locate(ctx context.Context, partition string, block uint64) error {
	number, err := partitionNumber(partition)
	if err != nil {
		return err
	}

	return scsiLocate(ctx, r.file, number, block)
}

// ReadBlock reads the block at the current position and advances one block,
// returning a copy of the block's payload bytes.
func (r *RawReader) ReadBlock(ctx context.Context) ([]byte, error) {
	return scsiReadBlock(ctx, r.file, r.bufSize)
}

const (
	opcodeLocate16 = 0x92 // LOCATE(16)
	locate16CP     = 0x02 // CP (change partition) bit in LOCATE(16) byte 1

	sgSetReservedSize = 0x2275 // SG_SET_RESERVED_SIZE ioctl
	sgGetReservedSize = 0x2272 // SG_GET_RESERVED_SIZE ioctl

	senseKeyNoSense = 0x00 // SCSI sense key NO SENSE (informational conditions)

	// rawReadBufSize bounds a single block read. It is the LTFS default block size
	// (512 KiB) and also the largest single READ transfer the sg path accepts on
	// this hardware — probing shows 512 KiB succeeds and 1 MiB fails with EINVAL.
	// Because a block cannot exceed the largest transfer the drive will do in one
	// READ, a read that fills the whole buffer is a complete block, not a
	// truncated one; LTFS cannot have written a block this path cannot read.
	rawReadBufSize = 512 * 1024

	// LTFS labels the index partition "a" and the data partition "b" (LTFS
	// Format Specification 2.4, §8). Their physical SCSI partition numbers below
	// are the reference-ltfs / mkltfs default; the data-partition value ("b", the
	// one archive bytes live in) is verified end-to-end against the mhvtl virtual
	// library in the index-loss recovery integration test.
	ltfsIndexPartition = 0 // "a"
	ltfsDataPartition  = 1 // "b"
)

// locateTimeout bounds a LOCATE: repositioning across a cartridge can wind for a
// while on real hardware, so allow generously.
const locateTimeout = 5 * time.Minute

// growReservedSize asks the sg driver to grow this file's reserved transfer
// buffer to want bytes and returns the size the kernel actually granted (it
// clamps to its own maximum). That granted size is the largest single block a
// READ can transfer on this handle.
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

// partitionNumber maps an LTFS partition label to its physical SCSI partition
// number.
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

// scsiLocate issues a SCSI LOCATE(16) to position at (partition, block) and
// waits for completion. CP is set so the partition field selects the partition;
// DEST_TYPE is 0, so the logical identifier is a logical block address.
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

// scsiReadBlock issues a variable-length READ(6) (SILI set) for the block at the
// current position and returns a copy of the payload. Only a GOOD status carries
// a data block, and a CHECK CONDITION whose sense key is NO SENSE is the
// incorrect-length indication for a short data block (still a data phase); any
// other status, a real sense key, or a filemark means the position holds no
// readable data block — the extractor reads only recorded data blocks, so that
// is a mis-locate, reported as an error rather than silent truncation.
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

// decodeReadBlock interprets a completed READ(6) SG_IO result and returns a copy
// of the block payload, or an error if the position holds no readable data
// block. It is the pure decision half of scsiReadBlock, split out so the decode
// logic is unit-testable without a real drive (status-only completions cannot be
// produced by mhvtl). sense is the written sense bytes (sense[:sbLenWr]); buf is
// the READ data buffer; name identifies the device in error messages.
func decodeReadBlock(hdr *sgIOHdr, sense, buf []byte, name string) ([]byte, error) {
	if hdr.hostStatus != 0 || hdr.driverStatus&^driverSense != 0 {
		return nil, fmt.Errorf("READ(6) on %s transport error (host=0x%x driver=0x%x)",
			name, hdr.hostStatus, hdr.driverStatus)
	}

	// Only GOOD carries a data block, and a CHECK CONDITION carries the sense the
	// key check below interprets. Any other status byte (BUSY, RESERVATION
	// CONFLICT, TASK SET FULL, ...) means no data phase occurred: the residual and
	// the empty sense buffer would otherwise be misread as a full transfer, so
	// reject it before the sense/transferred checks rather than fabricate a block.
	if hdr.status != statusGood && hdr.status != statusCheckCondition {
		return nil, fmt.Errorf("READ(6) on %s returned no data block "+
			"(status=0x%02x)", name, hdr.status)
	}

	key, asc, ascq, filemark := parseSense(sense)
	transferred := int(hdr.dxferLen) - int(hdr.resid)

	// A CHECK CONDITION carrying sense key NO SENSE is informational, not an
	// error: it is the incorrect-length indication for a short block (LTFS packs
	// small files into sub-block records, so a data block can be shorter than the
	// format block size), and the data is still transferred. Any real sense key
	// (e.g. BLANK CHECK past end-of-data, MEDIUM ERROR) or a filemark means the
	// position holds no readable data block — a mis-locate.
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
