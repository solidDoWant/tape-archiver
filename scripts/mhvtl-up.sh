#!/usr/bin/env bash
# mhvtl-up.sh — start the mhvtl virtual tape library.
#
# Requires:
#   - $MHVTL_KO set to the built mhvtl.ko path (set automatically by `nix develop`)
#   - vtllibrary, vtltape, vtlcmd, make_vtl_media, mktape in PATH (from devShell)
#   - mtx in PATH (from devShell)
#   - sudo access for module loading and device permission changes
#
# Virtual library mirrors the production setup (SPEC.md §3):
#   1 library (changer), 2 LTO-6 drives, 47 storage slots, 3 I/O slots
#   Tapes barcoded TA0001L6–TA0047L6

set -euo pipefail

MHVTL_CONFIG_DIR="${MHVTL_CONFIG_DIR:-/etc/mhvtl}"
MHVTL_HOME_DIR="${MHVTL_HOME_DIR:-/opt/mhvtl}"

# Library/drive IDs match the device.conf below.
LIBRARY_ID=10
DRIVE0_ID=11
DRIVE1_ID=12

# ---------------------------------------------------------------------------
# Sanity checks
# ---------------------------------------------------------------------------

if [ -z "${MHVTL_KO:-}" ]; then
  echo "error: MHVTL_KO is not set — run this script from within 'nix develop'" >&2
  exit 1
fi
if [ ! -f "$MHVTL_KO" ]; then
  echo "error: kernel module not found: $MHVTL_KO" >&2
  exit 1
fi
for cmd in vtllibrary vtltape vtlcmd make_vtl_media mktape mtx; do
  command -v "$cmd" > /dev/null 2>&1 || {
    echo "error: '$cmd' not found in PATH — run this script from within 'nix develop'" >&2
    exit 1
  }
done

if lsmod | grep -q '^mhvtl '; then
  echo "mhvtl module already loaded; mhvtl-up is a no-op"
  exit 0
fi

# ---------------------------------------------------------------------------
# Kernel modules
# ---------------------------------------------------------------------------

echo "==> Loading kernel modules..."
sudo modprobe st
sudo modprobe ch
# sg backs the SCSI generic nodes (/dev/sg*) used by sg_logs, sg_turs, and the
# SG_IO blank check.
sudo modprobe sg
sudo insmod "$MHVTL_KO"

# ---------------------------------------------------------------------------
# /etc/mhvtl config
# ---------------------------------------------------------------------------

echo "==> Writing /etc/mhvtl config..."
sudo mkdir -p "$MHVTL_CONFIG_DIR"

sudo tee "$MHVTL_CONFIG_DIR/mhvtl.conf" > /dev/null << 'EOF'
MHVTL_HOME_PATH=/opt/mhvtl
CAPACITY=500
VERBOSE=0
VTL_DEBUG=0
EOF

# device.conf: library 10 (changer) + 2 LTO-6 drives (11, 12).
# NAA bytes are arbitrary but must be unique per device.
sudo tee "$MHVTL_CONFIG_DIR/device.conf" > /dev/null << 'EOF'
VERSION: 5

Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00
 Vendor identification: STK
 Product identification: L700
 Unit serial number: XYZZY_A
 NAA: 10:22:33:44:ab:00:00:00
 Home directory: /opt/mhvtl
 PERSIST: False
 Backoff: 400
# fifo: /var/tmp/mhvtl

Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00
 Library ID: 10 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD6
 Product revision level: 1760
 Unit serial number: XYZZY_A1
 NAA: 10:22:33:44:ab:00:01:00
 Compression: factor 1 enabled 1
 Compression type: lzo
 Backoff: 400
# fifo: /var/tmp/mhvtl

Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00
 Library ID: 10 Slot: 02
 Vendor identification: IBM
 Product identification: ULT3580-TD6
 Product revision level: 1760
 Unit serial number: XYZZY_A2
 NAA: 10:22:33:44:ab:00:02:00
 Compression: factor 1 enabled 1
 Compression type: lzo
 Backoff: 400
# fifo: /var/tmp/mhvtl
EOF

# library_contents.<LIBRARY_ID>: slots, pickers, MAPs, and media.
# 47 storage slots mirror production; 3 MAPs = I/O slots; tapes TA0001L6–TA0047L6.
{
  printf 'Drive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\nMAP 2:\nMAP 3:\n\n'
  for i in $(seq 1 47); do
    printf 'Slot %d: TA%04dL6\n' "$i" "$i"
  done
} | sudo tee "$MHVTL_CONFIG_DIR/library_contents.${LIBRARY_ID}" > /dev/null

# ---------------------------------------------------------------------------
# Virtual media
# ---------------------------------------------------------------------------

echo "==> Creating virtual tape media in $MHVTL_HOME_DIR..."
sudo mkdir -p "$MHVTL_HOME_DIR"
# -f overwrites any stale media from a previous run.
# Pass PATH so sudo can find Nix-store binaries (mktape, etc.).
sudo env PATH="$PATH" make_vtl_media -H "$MHVTL_HOME_DIR" -C "$MHVTL_CONFIG_DIR" -c -f

# ---------------------------------------------------------------------------
# Start daemons
# ---------------------------------------------------------------------------

echo "==> Starting vtllibrary (ID $LIBRARY_ID)..."
sudo env PATH="$PATH" vtllibrary -F -q${LIBRARY_ID} -v0 &

echo "==> Starting vtltape (drive $DRIVE0_ID)..."
sudo env PATH="$PATH" vtltape -F -q${DRIVE0_ID} -v0 &

echo "==> Starting vtltape (drive $DRIVE1_ID)..."
sudo env PATH="$PATH" vtltape -F -q${DRIVE1_ID} -v0 &

# ---------------------------------------------------------------------------
# Wait for device nodes
# ---------------------------------------------------------------------------

echo -n "==> Waiting for device nodes"
for _ in $(seq 1 30); do
  if [ -e /dev/sch0 ] && [ -e /dev/nst0 ] && [ -e /dev/nst1 ]; then
    break
  fi
  echo -n "."
  sleep 0.5
done
echo ""

if [ ! -e /dev/sch0 ]; then
  echo "error: /dev/sch0 did not appear after 15 s — check daemon logs" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Device permissions
# dev nodes are owned root:cdrom / root:tape with 0660.
# Open them to all users for dev/test; a production setup would use udev rules.
# ---------------------------------------------------------------------------

echo "==> Setting device permissions..."
sudo chmod a+rw /dev/sch0
sudo chmod a+rw /dev/st* /dev/nst* 2>/dev/null || true
# SCSI generic nodes back the drives too; sg_logs / sg_turs (and the SG_IO
# blank check) open them directly. Without this they are root:tape 0660.
sudo chmod a+rw /dev/sg* 2>/dev/null || true

# ---------------------------------------------------------------------------
# Wait for daemons to be fully ready (device nodes appear before the daemon
# has processed the SCSI INQUIRY / REPORT ELEMENT STATUS commands)
# ---------------------------------------------------------------------------

echo -n "==> Waiting for library to become ready"
for _ in $(seq 1 20); do
  if sudo mtx -f /dev/sch0 status > /dev/null 2>&1; then
    break
  fi
  echo -n "."
  sleep 0.5
done
echo ""

# ---------------------------------------------------------------------------
# Status report
# ---------------------------------------------------------------------------

echo ""
echo "==> mhvtl virtual library is up. Device nodes:"
echo "    changer : /dev/sch0"
echo "    drive 0 : /dev/nst0  (non-rewinding)"
echo "    drive 1 : /dev/nst1  (non-rewinding)"
echo ""
echo "==> Library status:"
sudo mtx -f /dev/sch0 status
