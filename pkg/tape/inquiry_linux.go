//go:build linux

package tape

import (
	"context"
	"os"
	"time"
	"unsafe"
)

// This file issues SCSI INQUIRY directly via SG_IO, reusing the cgo-free plumbing
// shared with the changer and blank-check code (sgIOHdr, sgCommand, withSGFile,
// timeoutMs, and the dxfer constants). The response decoders it feeds are in the
// platform-independent inquiry.go.

const (
	opcodeInquiry = 0x12 // INQUIRY
	inquiryEVPD   = 0x01 // EVPD bit in INQUIRY byte 1 (request a vital product data page)
	vpdUnitSerial = 0x80 // Unit Serial Number VPD page code

	// inquiryBufSize bounds the INQUIRY transfer. The standard INQUIRY identity
	// strings end at byte 36 and a unit serial number is short; 252 is the
	// conventional allocation length and leaves ample headroom.
	inquiryBufSize = 252

	// inquiryTimeout bounds an INQUIRY. It is a control command answered from the
	// device's own tables with no media motion, so it returns near-instantly; the
	// timeout only bounds a wedged bus.
	inquiryTimeout = 30 * time.Second
)

// Inquire reads the drive's SCSI identity (vendor/product/revision and unit
// serial) via INQUIRY, for recording as provenance in the run report (SPEC §9).
// INQUIRY is answered by the drive itself with no media motion, so no tape need
// be loaded and the tape is never repositioned.
func (d *Drive) Inquire(ctx context.Context) (DeviceInfo, error) {
	sgDevice, err := d.SGDevice()
	if err != nil {
		return DeviceInfo{}, err
	}

	return inquireSG(ctx, sgDevice)
}

// Inquire reads the library/changer's SCSI identity (vendor/product/revision and
// unit serial) via INQUIRY, for recording as provenance in the run report.
func (c *Changer) Inquire(ctx context.Context) (DeviceInfo, error) {
	sgDevice, err := c.sgNode()
	if err != nil {
		return DeviceInfo{}, err
	}

	return inquireSG(ctx, sgDevice)
}

// inquireSG issues a standard INQUIRY on the sg node and decodes the identity,
// then makes a best-effort read of the Unit Serial Number VPD page (0x80): not
// every device implements it, so a failure there leaves Serial empty rather than
// failing the whole INQUIRY.
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

// scsiInquiry issues INQUIRY (0x12) — the standard page when evpd is false, or
// the given vital-product-data page when true — and returns the response bytes
// actually transferred.
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

	if err := sgCommand(f, &hdr, cdb, sense, buf, "INQUIRY"); err != nil {
		return nil, err
	}

	return buf[:int(hdr.dxferLen)-int(hdr.resid)], nil
}
