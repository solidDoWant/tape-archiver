//go:build linux

package tape

import (
	"context"
	"os"
	"time"
	"unsafe"
)

const (
	opcodeInquiry = 0x12 // INQUIRY
	inquiryEVPD   = 0x01 // EVPD bit in INQUIRY byte 1 (request a vital product data page)
	vpdUnitSerial = 0x80 // Unit Serial Number VPD page code

	inquiryBufSize = 252

	// inquiryTimeout only bounds a wedged bus; INQUIRY involves no media motion.
	inquiryTimeout = 30 * time.Second
)

// Inquire reads the drive's SCSI identity for the report (SPEC §9). It needs no
// tape and never repositions one.
func (d *Drive) Inquire(ctx context.Context) (DeviceInfo, error) {
	sgDevice, err := d.SGDevice()
	if err != nil {
		return DeviceInfo{}, err
	}

	return inquireSG(ctx, sgDevice)
}

// Inquire reads the changer's SCSI identity for the report.
func (c *Changer) Inquire(ctx context.Context) (DeviceInfo, error) {
	sgDevice, err := c.sgNode()
	if err != nil {
		return DeviceInfo{}, err
	}

	return inquireSG(ctx, sgDevice)
}

// inquireSG reads the standard INQUIRY plus a best-effort VPD 0x80 serial, which
// not every device implements.
func inquireSG(ctx context.Context, sgDevice string) (DeviceInfo, error) {
	var info DeviceInfo

	err := withSGFile(sgDevice, func(f *os.File) error {
		std, err := scsiInquiry(ctx, f, false, 0)
		if err != nil {
			return err
		}

		info.Vendor, info.Product, info.Revision = decodeStandardInquiry(std)

		if serial, err := scsiInquiry(ctx, f, true, vpdUnitSerial); err == nil {
			info.Serial = decodeUnitSerial(serial)
		}

		return nil
	})
	if err != nil {
		return DeviceInfo{}, err
	}

	return info, nil
}

func scsiInquiry(ctx context.Context, f *os.File, evpd bool, page byte) ([]byte, error) {
	buf := make([]byte, inquiryBufSize)
	sense := make([]byte, senseBufferLen)

	var evpdBit byte
	if evpd {
		evpdBit = inquiryEVPD
	}

	alloc := uint16(len(buf))
	cdb := []byte{opcodeInquiry, evpdBit, page, byte(alloc >> 8), byte(alloc), 0}

	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferFromDev,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		dxferLen:       uint32(len(buf)),
		dxferp:         uintptr(unsafe.Pointer(&buf[0])),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        timeoutMs(ctx, inquiryTimeout),
	}

	if err := sgCommand(ctx, f, &hdr, cdb, sense, buf, "INQUIRY"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}
