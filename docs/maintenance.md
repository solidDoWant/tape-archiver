# Maintenance

Long-term cold storage is only as good as its upkeep. tape-archiver produces offline
artifacts — LTO tapes and optical recovery discs — that must stay readable for ~20 years
(SPEC §2). This guide covers the recurring operator tasks that keep them recoverable:
recovery-disc refresh, tape rotation, and the barcode labelling convention.

None of these tasks touch a running workflow; they are physical-media housekeeping done
between runs. For the run-time operator loops (restocking blanks after a Load/Write
failure, clearing a full I/O station), see
[configuration.md](configuration.md#library).

---

## Recovery-disc re-burn and verification cadence

The optical recovery disc ([recovery-iso.md](recovery-iso.md)) is a **redundancy layer,
not a hard dependency**: every archive is also `age`-sealed, PAR2-protected, and
SHA-256-checksummed on the tapes themselves, and the PDF report carries the same recovery
material. The disc's job is to make recovery *convenient and self-contained* decades from
now, so keeping it readable is a maintenance task, not an emergency.

**Target media is M-DISC DVD** (SPEC §10): its inorganic recording layer is ISO/IEC
10995-tested and NIST-listed for 100+ year archival life. The rated media life is not the
real risk — drive availability, handling damage, and slow readability drift are. Treat the
100-year figure as headroom, not a licence to ignore the discs.

Cadence:

- **Verify annually.** Once a year, read each burned disc back and check it against its
  `manifest.sha256` (the disc-content manifest `pkg/recoverykit` records and the Burn
  phase verifies against). Every file must read cleanly and match its digest. A disc that
  reads slowly, throws read errors, or mismatches a digest has begun to fail — re-burn it
  now (see below), do not wait for the 5-year mark.
- **Re-burn every 5 years, or immediately on any failure.** Burn a fresh disc from the
  archived ISO (or re-run the burn against a known-good copy) at least every five years,
  and at once whenever a verification fails or a disc is physically damaged or lost.
- **Always keep ≥ 2 copies in separate physical locations.** Burn at least two discs per
  run (SPEC §10) and store them apart, so a fire, flood, or theft at one site never
  destroys the last copy. When you re-burn, refresh **both** copies and keep them in their
  separate locations.

Burning and read-back verification can run **in the workflow** when
[`delivery.opticalBurn`](configuration.md#opticalburn) is configured — the Burn phase
burns each disc and verifies it against the disc-content manifest, pausing for the
operator between burn-sets (see
[configuration.md](configuration.md#optical-burn-operator-loop)). Left unconfigured,
burning and verification are a **manual operator step**: burn at least two copies and
verify each against `manifest.sha256`. Either way the re-burn/refresh above stays a
recurring maintenance task.

> **M-DISC is write-once.** A re-burn always uses a **fresh blank** disc; you cannot
> overwrite an M-DISC or DVD-R. Discard superseded discs securely — the disc carries the
> `age` private identity (SPEC §7), so a discarded recovery disc is a decryption secret in
> the bin. Destroy it, do not simply throw it away.

---

## Tape slot rotation

A run consumes the blank tapes in the storage slots listed in
[`library.blankSlots`](configuration.md#library) — one blank per physical tape written
(logical tapes × `copies`). Written tapes leave the drives and are ejected to the
library's import/export (I/O) station for you to remove; when more tapes are written than
the station has slots, the Eject phase fills the station and **pauses** for you to clear
it (bounded by [`ioWaitTimeoutSeconds`](configuration.md#library); on libraries that do
not report the I/O access bit you resume it with [`tapectl resume`](tapectl.md)). Rotation
is the routine of emptying that station and restocking blanks between runs.

After each run:

1. **Remove the written tapes** from the I/O station. Confirm each against the run's PDF
   report, which maps every archive to the tape barcode(s) that hold it.
2. **Store them for the long term, keeping the copies apart.** Each run writes `copies`
   identical tapes (production default 2; see [`copies`](configuration.md#top-level-fields));
   store the copies of a given logical tape in **separate physical locations** so no single
   site loss takes out every copy (SPEC §2, principle 3: plan for media failure). Handle
   tapes edge-on, keep them upright, and store them cool, dry, and away from strong
   magnetic fields.
3. **Restock the blank slots.** Load fresh, genuinely blank tapes into the slots named in
   `library.blankSlots` for the next run. A run **never writes a non-blank tape** unless
   [`allowNonBlankTapes`](configuration.md#library) is deliberately set, so a
   mistakenly-restocked used tape fails the run early rather than being overwritten —
   restock only known-blank media.
4. **Keep the barcode inventory current.** Record which barcodes were written, what they
   hold (cross-referenced to the run and report), and where each copy is stored. The run
   holds no cross-run state and there is no online catalog (SPEC §4.2), so this inventory
   is the operator's own record and the primary index for a future recovery.

This procedure is library-agnostic — it depends only on the changer's storage and I/O
slots, not on any particular library model.

---

## Barcode label convention

The library-read **barcode is the canonical physical tape ID** (SPEC §6): the SCSI volume
tag the changer reads is authoritative, `mkltfs` sets each tape's LTFS volume name to its
barcode, and the report and per-tape manifest reference tapes by barcode. Any human-facing
prefix or sequence is therefore **cosmetic** — it must simply match the physical label the
library actually reads.

The convention this project uses:

```
TA<NNNN>L<gen>
```

- **`TA`** — a fixed project prefix (Tape Archiver), so archive tapes are recognizable at
  a glance.
- **`<NNNN>`** — a zero-padded four-digit sequence number, assigned once per physical
  tape and never reused: `0001`, `0002`, ….
- **`L<gen>`** — the standard LTO media-generation suffix: `L6` for LTO-6, `L7` for LTO-7,
  `L8` for LTO-8, and so on. This is part of the conventional LTO barcode format, so
  off-the-shelf pre-printed LTO labels already carry it.

For example, `TA0001L6` is the first LTO-6 tape. Apply the printed barcode label to the
tape before its first use and record the barcode in the inventory above.

Because the physical label is what matters, the sequence is just for human bookkeeping:
the software identifies tapes by whatever the changer reads, so a tape relabelled or
sourced with a different scheme still works as long as its printed barcode and the library
agree. The dev/test virtual library (`mhvtl`) uses barcodes `TA0001L6`–`TA0047L6`,
matching this convention.
