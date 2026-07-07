# Recovery procedure

This is the authoritative, step-by-step procedure for recovering data from
tape-archiver tapes. It is written for a future operator who has **only the
physical tapes, the laminated PDF report, and one recovery disc** — no network,
no access to this repository, and not the original host. A copy of this document
ships on every recovery disc as `recovery-procedure.md`, and a concise version is
printed in the PDF report.

Everything here uses open, long-lived standards (`tar`, `age`, PAR2, LTFS,
ISO 9660) so the data stays recoverable decades from now (SPEC §2).

> **The tapes do not depend on LTFS being readable.** Every archive is
> `age`-encrypted, PAR2-protected, and SHA-256-checksummed, and the report lists
> every file. LTFS and its index are a convenience layer, not load-bearing — the
> [index-loss recovery](#index-loss-recovery) section below recovers data with no
> working on-tape index and no LTFS mount at all.

## What you have

- **The physical tapes**, each identified by its **barcode** (e.g. `TA0001L6`).
  The report maps every barcode to the archives it holds. When the run was
  configured for more than one copy, each logical tape is written to **two or
  more tapes**, so a damaged tape has a sibling; the report lists every copy by
  barcode, so consult it to see whether a given tape has one.
- **The laminated PDF report** (also on the disc as `report.pdf`). It lists the
  contents manifest (every archive, its source snapshot, sizes, SHA-256), the
  barcode-to-archive mapping, the **build parameters** (the exact `age`, `par2`,
  and `ltfs` versions and the tape/library identifiers used — the disc's
  `bin/zstd` and `bin/tar` are the exact binaries this run wrote with and
  self-report their versions), the **age private identity**, and a concise copy
  of this procedure.
- **The recovery disc**, an ISO 9660 image holding:
  - `report.pdf` — the report above.
  - `manifest.sha256` — the SHA-256 of every on-tape file.
  - `recovery-procedure.md` — this document.
  - `ltfs-index/<barcode>.schema` — a backup copy of each tape's LTFS index (one
    per tape), used for [index-loss recovery](#index-loss-recovery). The
    `<barcode>` is folded to **lowercase** on the mounted disc (e.g.
    `ltfs-index/ta0001l6.schema` for barcode `TA0001L6`), so a scripted
    exact-case lookup must lowercase the barcode.
  - `bin/<name>` — the static recovery binaries `age`, `par2`, `zstd`, `tar`.

## Prerequisites

A Linux host with a standalone **LTO tape drive** of the generation named in the
report's build parameters (a newer generation that can read that media also
works). Prefer the non-rewinding node (`/dev/nst0`) for the data path and note
the paired SCSI generic node (`/dev/sg0`).

**Tools that ship on the disc** under `bin/` — statically linked, run them
directly, no installation. These are the **exact binaries this run wrote with**,
so there is nothing to version-match — just run them:

| Tool | Purpose |
|------|---------|
| `bin/age` | decrypt archives (post-quantum recipients) |
| `bin/par2` | verify and repair with the PAR2 recovery set |
| `bin/zstd` | decompress (for compressed sources) |
| `bin/tar` | unpack the archive |

The exact `age`, `par2`, and `ltfs` versions this run used are recorded in the
report's **Build parameters** section — the authoritative record, so this
document does not restate version numbers that could drift from the shipped tools.

**Tools the host must provide** (not on the disc — install from any
distribution, or use the copies bundled in the data-worker image):

- **`ltfs`** — mount the tape (normal path) and `ltfsck` (tier-1 index repair).
  Install the version named in the report's **Build parameters** section (or a
  newer one that can read that format); `ltfs` is the one tool whose version must
  match the tape.
- **`mt` (`mt-st`)** and coreutils **`dd`**, **`cat`**, **`sha256sum`** — used by
  the [tier-2 index-loss](#tier-2--on-tape-index-unusable-captured-index-recovery)
  path to position the tape and read raw blocks.
- **`sg3-utils`** (`sg_raw`) — needed for the tier-2 path when the host's `st`
  driver does not support tape-partition positioning (`mt setpartition` then
  reports an error), so the partition must be selected with a raw SCSI
  `LOCATE(16)`. `lsscsi` (optional) helps identify device nodes. Neither is needed
  for the normal path or tier-1.

## Step 0 — Recover the decryption key

The archives are encrypted to post-quantum `age` recipients. The matching
**private identity** (`AGE-SECRET-KEY-PQ-1…`) is printed in `report.pdf` under
**Encryption key** and is the same secret on every disc for the run. Save it to a
file and keep it private:

```
umask 077
printf '%s\n' 'AGE-SECRET-KEY-PQ-1…' > identity.txt   # copy from report.pdf
```

This one identity decrypts every archive in the run. Treat `identity.txt`, the
report, and the disc as secrets — they carry the decryption key.

## Normal recovery (LTFS mounts cleanly)

Use this when the tape's on-tape LTFS index is intact. To pull **one specific
file** out of an archive:

1. **Load the tape** and confirm its barcode against the report to find which
   `archives/NNN-<label>/` directory holds the file's source snapshot. `NNN` is the
   zero-padded source index and `<label>` a descriptive name for the source; the
   report maps each directory to its source. (In the commands below, substitute the
   actual directory name for `NNN-<label>`.)
2. **Mount the volume read-only** (LTFS uses the SCSI generic node):

   ```
   mkdir -p /mnt/tape
   ltfs -o ro -o devname=/dev/sg0 /mnt/tape
   ```

3. **Copy the archive to local disk in one pass, then unmount the tape.** Do
   every later step from the local copy — never repeatedly off the tape:

   ```
   mkdir -p /scratch
   cp -r /mnt/tape/archives/NNN-<label> /scratch/   # -> /scratch/NNN-<label>
                                              # (or `cp -r /mnt/tape/* /scratch/` to take the whole tape)
   fusermount -u /mnt/tape                     # the tape is no longer needed
   ```

   Copy the target archive's whole directory — all of its `archive.NNN` slices
   **and** its `archive.par2` set — or the entire tape if you are recovering
   everything and have the disk space (up to one tape's capacity, e.g. ~2.5 TB for
   LTO-6). This first copy matters for two reasons:

   - **It minimizes tape wear.** The verify, repair, and reassemble steps below
     each read the archive; running them straight off the mount drags the drive
     back and forth over the same tape (repositioning / "shoe-shining"). Copying
     once is a single sequential streaming pass — the read-side mirror of how the
     archiver *writes* (SPEC §2, principle 2). Once the copy is on disk you can
     retry freely without touching the tape again.
   - **PAR2 repair needs a writable copy.** `par2 repair` rewrites the repaired
     slice in place, which cannot be done on the read-only tape mount.

4. **Verify and repair with PAR2.** `par2 repair` checks every slice against the
   recovery set and reconstructs the exact original bytes when damage is within
   the PAR2 redundancy. The `-p` flag purges PAR2's own artifacts once the repair
   succeeds — the `.par2` volume files and any `archive.NNN.1` pre-repair backups
   it makes of a damaged slice — leaving only the clean slice files so the
   reassembly glob in step 5 matches them and nothing else (this only touches the
   writable staging copy; the tape is untouched, so you can always re-stage and
   retry):

   ```
   bin/par2 repair -p /scratch/NNN/archive.par2
   ```

   As an independent SHA-256 cross-check you have two manifests. The tape's own
   `manifest.json` lists each file's digest relative to the archive, so it checks
   straight from the local copy. The disc's `manifest.sha256` is a single file
   spanning **every** tape, and each of its lines is prefixed with the source
   tape's barcode — `<barcode>/archives/NNN-<label>/<file>`. To use it, lay the
   copied files out under a directory named for that barcode (matching the
   manifest's paths) and run `sha256sum -c` from the parent of the barcode
   directory. Because one manifest covers all tapes, filter to the tape you have
   copied (works on any coreutils):

   ```
   mkdir -p /scratch/verify/<barcode>
   cp -r /mnt/tape/archives /scratch/verify/<barcode>/
   cd /scratch/verify
   grep '  <barcode>/' /path/to/disc/manifest.sha256 | sha256sum -c -
   ```

   Without the `grep` filter, `sha256sum -c /path/to/disc/manifest.sha256` also
   checks lines for tapes you have not copied and reports them as missing (a
   non-zero exit); on modern coreutils `sha256sum -c --ignore-missing` verifies
   everything copied so far in one pass.

5. **Reassemble the encrypted archive** by concatenating its slice files in
   numeric order (`manifest.json` lists them; they are named `archive.000`,
   `archive.001`, …). Every slice in one archive shares a single zero-padded
   suffix width — three digits by default, widened to fit the archive's slice
   count (e.g. `archive.0000` … `archive.1004` once there are more than 1000
   slices) — so the glob below expands in numeric order. `manifest.json` remains
   authoritative if you ever need to confirm the order:

   ```
   cat /scratch/NNN/archive.[0-9]* > /scratch/archive.age
   ```

6. **Decrypt** with the escrowed identity:

   ```
   bin/age -d -i identity.txt -o /scratch/archive.tar.zst /scratch/archive.age
   ```

7. **Decompress** if the source was stored compressed (the report's contents
   manifest records this per source; an uncompressed archive is already a `tar`
   after step 6):

   ```
   bin/zstd -d /scratch/archive.tar.zst -o /scratch/archive.tar
   ```

8. **Extract the specific file** you need (omit the path to extract everything).
   A `VolumeGroupSnapshot` archive unpacks to one subdirectory per member
   volume:

   ```
   bin/tar -xf /scratch/archive.tar path/inside/the/archive
   ```

   **What a `tar`-level restore reproduces:** regular files, directories, and
   symlinks, with their permission bits, ownership, and modification time.
   Hardlinked files are restored as hardlinks (stored once in the archive), and
   sparse files are restored with their holes intact — the bundled `bin/tar`
   decodes the GNU sparse 1.0 encoding used to write them. **Not** reproduced:
   extended attributes (`user.*`/`security.*`), POSIX ACLs, and file capabilities
   (`security.capability`, e.g. `setcap` binaries) are not carried by the archive,
   so a restored file has its contents and mode bits but none of that extra
   metadata. Unix sockets, device nodes, and named pipes present in the source
   snapshot were skipped at backup time and are absent from the archive.

## Index-loss recovery

LTFS may fail to mount because its **on-tape index is damaged**. Recover in two
tiers, from lightest to last-resort. Both are automated-test-backed against the
virtual library.

### Tier 1 — index partition damaged, data partition intact

LTFS writes the index to **both** partitions on every sync (the index partition
`a`, and a copy at the tail of the data partition `b`). If only the index
partition is damaged, rebuild from the data-partition copy:

```
ltfsck --deep-recovery /dev/sg0
```

Then mount read-only and follow [Normal recovery](#normal-recovery-ltfs-mounts-cleanly).

### Tier 2 — on-tape index unusable (captured-index recovery)

When LTFS will not mount at all, recover **without any working on-tape index and
without mounting LTFS**, using the captured index shipped on the disc:
`ltfs-index/<barcode>.schema` (the `<barcode>` component is lowercased on the
disc, e.g. `ta0001l6.schema`). That XML is a complete byte-level map — for every
file it lists one or more **extents**:

```xml
<extent>
  <partition>b</partition>     <!-- LTFS partition: a = index, b = data -->
  <startblock>11</startblock>  <!-- first tape block of this extent -->
  <byteoffset>0</byteoffset>   <!-- offset into that block where data begins -->
  <bytecount>44</bytecount>    <!-- number of bytes in this extent -->
  <fileoffset>0</fileoffset>   <!-- where these bytes sit within the file -->
</extent>
```

To reconstruct each of the target archive's slice files (`archive.000`, …), for
each extent listed under that file in the captured index:

1. **Select the partition and position to the start block.** Map the LTFS
   partition label to the drive partition number — data `b` = partition `1`,
   index `a` = partition `0`. This mapping is fixed: the archiver formats every
   tape with `mkltfs` defaults, so the reference two-partition layout always
   holds. If the host's `st` driver supports tape partitions:

   ```
   mt -f /dev/nst0 setpartition 1
   mt -f /dev/nst0 seek <startblock>
   ```

   If `mt` reports the partition operation is unsupported (some drivers and
   virtual libraries do), position with a raw SCSI `LOCATE(16)` instead — this is
   the method the automated recovery test uses:

   ```
   # byte 3 selects the partition (00 = index "a", 01 = data "b");
   # bytes 4-11 are the start block, 8 bytes big-endian.
   sg_raw /dev/sg0 92 02 00 01  00 00 00 00 00 00 00 <startblock>  00 00 00 00
   ```

2. **Read the raw block(s).** Read whole tape blocks (the LTFS format block size
   is `524288` = 512 KiB, the `mkltfs` default the archiver formats with) until
   you have at least `byteoffset + bytecount` bytes:

   ```
   dd if=/dev/nst0 bs=524288 count=<blocks-needed> of=extent.raw
   ```

3. **Slice out the extent's bytes** — the `bytecount` bytes starting at
   `byteoffset` — and append them into the slice file at `fileoffset`. Process a
   file's extents in `fileoffset` order.

4. **Reassemble and finish.** You have now written the slice files to local disk,
   so continue exactly as in [Normal recovery](#normal-recovery-ltfs-mounts-cleanly)
   from step 4: PAR2-repair, `cat` the slices into `archive.age`, then `age`
   decrypt, `zstd` decompress, and `tar` extract.

The extents alone are sufficient to recover the exact archive bytes; this is
proven end-to-end by an automated test that corrupts the on-tape index and
rebuilds a file from the captured extents plus raw block reads.

## Failure scenarios and handling

For failures that happen **during a backup run** (a Load/Write pause, the I/O
station filling, a non-blank-tape refusal, or an optical-burn failure) see
[configuration.md](configuration.md) and [tapectl.md](tapectl.md); those are
operator-in-the-loop and resolved with `tapectl resume`/`abort`. The scenarios
below are **recovery-time**.

| Symptom | What it means | What to do |
|---------|---------------|------------|
| A slice or PAR2 file fails its checksum, but only a little | Media damage **within** the PAR2 redundancy | `bin/par2 repair archives/NNN-<label>/archive.par2` reconstructs the exact bytes; continue. |
| `par2 repair` cannot repair (damage **exceeds** PAR2 capacity) | Too much of one region is gone | Recover that archive from **another copy, if the run wrote one** (the report lists every copy by barcode). Blast radius is bounded — one bad region damages at most one slice. |
| `age -d` aborts partway with a stream error | `age` authenticates each ~64 KiB chunk and stops at the first uncorrectable one, so the archive is truncated there | PAR2-repair first; if that is not enough, decrypt **another copy, if the run wrote one** (the report lists them by barcode). |
| A file's digest does not match `manifest.sha256` / on-tape `manifest.json` | That file is corrupt | Identify the slice, `par2 repair` it, or use another copy if the run wrote one. |
| A tape has **no `manifest.json`** at its LTFS root | The tape was **not completely written** (the manifest is written last as the completeness signal) | Discard that tape and recover from another copy if the run wrote one. |
| The barcode label is unreadable | You cannot identify the tape by its label | The LTFS **volume name equals the barcode** (`mkltfs` sets it); read it from the mounted volume or the captured index, then cross-reference the report. |

## See also

- [report.md](report.md) — what the PDF report contains, including the key escrow.
- [recovery-iso.md](recovery-iso.md) — the recovery disc's full contents.
- [maintenance.md](maintenance.md) — recovery-disc re-burn cadence and tape rotation.
