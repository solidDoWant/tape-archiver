package tape

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Changer wraps the mtx tape library changer tool.
type Changer struct {
	device string
}

// NewChanger returns a Changer targeting the given device (e.g. /dev/sch0).
func NewChanger(device string) *Changer {
	return &Changer{device: device}
}

// Inventory queries the changer with "mtx status" and returns the parsed result.
func (c *Changer) Inventory(ctx context.Context) (Inventory, error) {
	out, err := c.output(ctx, "status")
	if err != nil {
		return Inventory{}, err
	}

	inv, err := parseInventory(string(out))
	if err != nil {
		return Inventory{}, fmt.Errorf("parse mtx status: %w", err)
	}

	return inv, nil
}

// Load moves the tape in the given storage slot into the given drive (0-indexed).
func (c *Changer) Load(ctx context.Context, slot, drive int) error {
	_, err := c.output(ctx, "load", strconv.Itoa(slot), strconv.Itoa(drive))

	return err
}

// Unload moves the tape from the given drive (0-indexed) into the given slot.
func (c *Changer) Unload(ctx context.Context, slot, drive int) error {
	_, err := c.output(ctx, "unload", strconv.Itoa(slot), strconv.Itoa(drive))

	return err
}

// Transfer moves media from srcSlot to dstSlot (both are element addresses).
// Use this to move a tape from a drive's home slot to an I/O station slot.
func (c *Changer) Transfer(ctx context.Context, srcSlot, dstSlot int) error {
	_, err := c.output(ctx, "transfer", strconv.Itoa(srcSlot), strconv.Itoa(dstSlot))

	return err
}

// output executes an mtx subcommand and returns its stdout. On failure the
// returned error names the exact command that ran — the resolved binary path
// via cmd.String() — and appends mtx's stderr, which carries the human-readable
// reason (e.g. "Drive 0 Full"). This is the single place that turns an mtx
// invocation into an error, so callers never reconstruct the command line by
// hand (which previously diverged from the real invocation and dropped stderr).
func (c *Changer) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.mtx(ctx, args...)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return nil, fmt.Errorf("%s: %w", cmd, err)
	}

	return out, nil
}

// mtx returns an exec.Cmd for "mtx -f <device> <args...>".
//
// The command is invoked directly; the program never escalates its own
// privilege. Issuing SCSI commands to the changer requires CAP_SYS_RAWIO plus
// access to the device node, which the operator grants by running the worker
// with the necessary privilege (root, or CAP_SYS_RAWIO and a device mount in
// the data-worker container). This matches the SG_IO blank check and the
// sg_logs path, which likewise require an already-privileged process — there is
// no way to elevate an in-process ioctl after the fact.
func (c *Changer) mtx(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "mtx", append([]string{"-f", c.device}, args...)...)
}

// parseInventory parses the output of "mtx -f <dev> status" into an Inventory.
//
// Example lines:
//
//	  Storage Changer /dev/sch0:2 Drives, 47 Slots ( 3 Import/Export )
//	Data Transfer Element 0:Empty
//	Data Transfer Element 1:Full (Storage Element 3 Loaded):VolumeTag=TA0003L6
//	      Storage Element 1:Full :VolumeTag=TA0001L6
//	      Storage Element 3:Empty
//	      Storage Element 48 IMPORT/EXPORT:Full :VolumeTag=TA0001L6
func parseInventory(output string) (Inventory, error) {
	var inv Inventory

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "Data Transfer Element "):
			el, err := parseDriveElement(line)
			if err != nil {
				return Inventory{}, err
			}

			inv.Drives = append(inv.Drives, el)

		case strings.HasPrefix(line, "Storage Element ") && strings.Contains(line, "IMPORT/EXPORT"):
			el, err := parseIOElement(line)
			if err != nil {
				return Inventory{}, err
			}

			inv.IOSlots = append(inv.IOSlots, el)

		case strings.HasPrefix(line, "Storage Element "):
			el, err := parseStorageElement(line)
			if err != nil {
				return Inventory{}, err
			}

			inv.Slots = append(inv.Slots, el)
		}
	}

	return inv, nil
}

// parseDriveElement parses lines like:
//
//	Data Transfer Element 0:Empty
//	Data Transfer Element 1:Full (Storage Element 3 Loaded):VolumeTag=TA0003L6
//	Data Transfer Element 0:Full (Storage Element 1 Loaded):VolumeTag = TA0001L6
//
// mhvtl uses "VolumeTag = value" (spaces around =) while some real changers
// omit the spaces; parseVolumeTag handles both.
func parseDriveElement(line string) (DriveElement, error) {
	// Strip prefix.
	rest := strings.TrimPrefix(line, "Data Transfer Element ")

	// Split on first colon to get address and the status part.
	addrStr, status, found := strings.Cut(rest, ":")
	if !found {
		return DriveElement{}, fmt.Errorf("unexpected drive element line: %q", line)
	}

	addr, err := strconv.Atoi(strings.TrimSpace(addrStr))
	if err != nil {
		return DriveElement{}, fmt.Errorf("parse drive address %q: %w", addrStr, err)
	}

	el := DriveElement{Address: addr}

	if strings.HasPrefix(status, "Empty") {
		return el, nil
	}

	if !strings.HasPrefix(status, "Full") {
		return DriveElement{}, fmt.Errorf("unexpected drive status in line: %q", line)
	}

	el.Loaded = true

	// Extract source slot if present: "Full (Storage Element 3 Loaded):VolumeTag=..."
	if idx := strings.Index(status, "(Storage Element "); idx >= 0 {
		rest2 := status[idx+len("(Storage Element "):]

		slotStr, _, _ := strings.Cut(rest2, " ")
		if slot, e := strconv.Atoi(slotStr); e == nil {
			el.SourceSlot = slot
		}
	}

	el.Barcode = parseVolumeTag(status)

	return el, nil
}

// parseStorageElement parses lines like:
//
//	Storage Element 1:Full :VolumeTag=TA0001L6
//	Storage Element 3:Empty
func parseStorageElement(line string) (StorageElement, error) {
	rest := strings.TrimPrefix(line, "Storage Element ")

	addrStr, status, found := strings.Cut(rest, ":")
	if !found {
		return StorageElement{}, fmt.Errorf("unexpected storage element line: %q", line)
	}

	addr, err := strconv.Atoi(strings.TrimSpace(addrStr))
	if err != nil {
		return StorageElement{}, fmt.Errorf("parse storage address %q: %w", addrStr, err)
	}

	el := StorageElement{Address: addr}

	if strings.Contains(status, "Full") {
		el.Full = true
		el.Barcode = parseVolumeTag(status)
	}

	return el, nil
}

// parseIOElement parses lines like:
//
//	Storage Element 48 IMPORT/EXPORT:Empty
//	Storage Element 49 IMPORT/EXPORT:Full :VolumeTag=TA0048L6
func parseIOElement(line string) (IOElement, error) {
	rest := strings.TrimPrefix(line, "Storage Element ")

	// Address is the number before the space.
	addrStr, after, found := strings.Cut(rest, " ")
	if !found {
		return IOElement{}, fmt.Errorf("unexpected IO element line: %q", line)
	}

	addr, err := strconv.Atoi(strings.TrimSpace(addrStr))
	if err != nil {
		return IOElement{}, fmt.Errorf("parse IO address %q: %w", addrStr, err)
	}

	_, status, found := strings.Cut(after, ":")
	if !found {
		return IOElement{}, fmt.Errorf("unexpected IO element line (no status): %q", line)
	}

	el := IOElement{Address: addr}

	if strings.Contains(status, "Full") {
		el.Full = true
		el.Barcode = parseVolumeTag(status)
	}

	return el, nil
}

// parseVolumeTag extracts the primary barcode from a status fragment containing
// "VolumeTag". Handles both "VolumeTag=TA0001L6" and "VolumeTag = TA0001L6"
// (mhvtl style).
//
// mtx appends each tag as its own colon-delimited field and, when the library
// reports one, follows the primary tag with ":AlternateVolumeTag=...". The
// fragment is therefore split on ":" and only the field that is exactly the
// primary VolumeTag is used — an AlternateVolumeTag field starts with "A" and is
// skipped, and bounding the value to its own field stops the alternate tag from
// bleeding into the barcode. SCSI volume tags printed by mtx contain neither ":"
// nor embedded spaces, so this split is lossless.
func parseVolumeTag(s string) Barcode {
	for _, field := range strings.Split(s, ":") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(field), "VolumeTag")
		if !ok {
			continue
		}

		_, value, found := strings.Cut(rest, "=")
		if !found {
			continue
		}

		return Barcode(strings.TrimSpace(value))
	}

	return ""
}
