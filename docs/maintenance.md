# Maintenance

Long-term cold storage is only as good as its upkeep. tape-archiver produces offline
artifacts — LTO tapes and optical recovery discs — that must stay readable for ~20 years
(SPEC §2). This guide covers the recurring operator tasks that keep them recoverable:
recovery-disc refresh, tape rotation, and the barcode labelling convention, plus clearing
anything a dead run leaked (holds, run-scoped snapshots, staging directories, a tape locked
in a drive).

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

- **Verify annually.** Once a year, read each burned disc back and confirm every file
  reads cleanly and matches the **archived master ISO** (`recovery.iso`) — the same durable
  source a re-burn uses (see below). A disc that reads slowly, throws read errors, or
  mismatches has begun to fail — re-burn it now, do not wait for the 5-year mark. (Note: the
  disc's own `manifest.sha256` is the SHA-256 manifest of the **on-tape** files, not of the
  disc's contents, so it cannot verify a disc file against itself. The Burn phase's own
  read-back check instead compares against a separate `disc-manifest.sha256` that
  `pkg/recoverykit` stages beside the uncompressed ISO in the run staging directory and does
  **not** ship on the disc.)
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
verify each disc reads back cleanly against the archived master ISO (`recovery.iso`).
Either way the re-burn/refresh above stays a recurring maintenance task.

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

## Clearing a leaked source-snapshot hold

For the duration of a run, tape-archiver pins each of its ZFS source snapshots with a
`zfs hold` so an external prune (`zfs destroy`) cannot delete a snapshot mid-run (SPEC
§4.3). The hold is tagged `tape-archiver-hold-<run-id>` — reconstructable from the run
alone, with no cross-run state (SPEC §4.2) — and is **released automatically on every
run exit**: success, failure, or cancellation, unless the release itself fails (below).

Two things can leave a hold behind:

- A hard **Terminate** of a run (as opposed to a cancel) skips the workflow's cleanup.
- An **exhausted release**. The release retries transient failures (a busy pool, a
  failing `zfs` command, a data-worker restart) with backoff from 1 s doubling to 1 min,
  up to 10 attempts and at most 15 minutes in total, including any time spent waiting
  for an unreachable data worker. If it still fails, the run gives up so the failure
  alert and staging cleanup are not blocked. The control worker then logs
  `failed to release source snapshot holds`, with the `tag` and the `error`. On a
  failed run, the failure alert's error summary also ends with
  `also: release source-snapshot hold tape-archiver-hold-<run-id> (leaked; clear it by hand): ...`.
  On a successful run there is no alert, so watch the logs for that line. A snapshot
  that was already destroyed is not a failure: releasing it is a no-op.

A leaked hold is harmless — it never blocks a future run,
whose hold carries a different run-id tag — but it keeps the affected snapshot
undestroyable until cleared, which can defeat an external snapshot-pruning policy. It is
fully observable and clearable by hand; there is no hidden state to reconcile:

1. **List the holds** on a snapshot you expect to be prunable:

   ```
   zfs holds bulk-pool-01/archive@daily-2026-06-28
   ```

   A leaked hold shows a `tape-archiver-hold-<run-id>` tag whose run is no longer active
   (cross-check the run id in Temporal).

2. **Release it** once you have confirmed the run is truly gone:

   ```
   zfs release tape-archiver-hold-<run-id> bulk-pool-01/archive@daily-2026-06-28
   ```

Only release a hold whose run has ended — releasing an in-flight run's hold removes the
very protection this feature provides.

A leaked hold on a **run-scoped** snapshot (`@tape-archiver-<run-id>`) needs no manual
release: the next run that takes run-scoped snapshots releases it and destroys the
snapshot (below).

---

## Removing leaked run-scoped snapshots

A bare `zfsPath` dataset, and every dataset a `zfsProperty` source selects, is archived
from a snapshot the run takes itself, `<dataset>@tape-archiver-<run-id>`, created with the
user property `tape-archiver:run-id=<run-id>` ([configuration.md](configuration.md#run-scoped-snapshots)).
The run destroys it on every exit (success, failure, cancellation) after releasing its
hold. It can still be leaked by a hard **Terminate**, or by a destroy that keeps failing
for its whole retry window (1 s doubling to 1 min, up to 10 attempts and 15 minutes). The
control worker then logs `failed to destroy run-scoped snapshots`, and a failed run's alert
ends with `also: destroy run-scoped snapshots [...] (leaked; ...)`.

Leaks clean themselves up: only one run is ever active (the fixed workflow id), so the next
run that takes run-scoped snapshots destroys every snapshot whose local
`tape-archiver:run-id` names another run and whose name is `tape-archiver-<that id>`,
releasing tape-archiver's own `tape-archiver-hold-*` holds on it first. It never touches a
snapshot without that property or one held by anything else; such a snapshot is logged and
left for you.

To remove leaks by hand (e.g. before a long gap between runs, or one held by another
tool):

1. **Confirm no run is active** — `tapectl status`, or the Temporal UI shows no running
   `backup` workflow. Destroying an in-flight run's snapshot fails its Prepare.

2. **List tape-archiver's snapshots** (only those carrying the property locally):

   ```
   zfs get -H -s local -t snapshot -o name,value tape-archiver:run-id
   ```

   Each line is `<snapshot>  <run-id>`; with no run active, every line is a leak. Check
   the snapshot name is `<dataset>@tape-archiver-<run-id>` for the listed run id.

3. **Release its holds, then destroy it**:

   ```
   zfs holds bulk-pool-01/archive@tape-archiver-<run-id>
   zfs release tape-archiver-hold-<run-id> bulk-pool-01/archive@tape-archiver-<run-id>
   zfs destroy bulk-pool-01/archive@tape-archiver-<run-id>
   ```

   Release a hold from another tool only if you know why it is there. Never destroy a
   `tape-archiver-*` snapshot that lacks the `tape-archiver:run-id` property — someone else
   made it.

---

## Reclaiming a leaked staging directory

Prepare stages a complete second copy of everything a run archives — encrypted archive
slices plus their PAR2 recovery sets — under `TAPE_STAGING_DIR` on the data worker (SPEC
§4.3, §14). That staged tree is reclaimed **automatically on every run exit**: success,
failure, or cancellation. What survives is deliberately small and durable: `report.pdf`,
and — when [optical burning](configuration.md#opticalburn) is enabled — `recovery.iso`
and `disc-manifest.sha256`, the same master ISO the
[annual disc-verification cadence](#recovery-disc-re-burn-and-verification-cadence)
checks a burned disc against. Those retained artifacts accumulate across runs by
design; they are not pruned automatically — treat them as an operator-managed archive of
past runs' reports and master ISOs, and delete old ones by hand once you no longer need
them.

A hard **Terminate** of a run (as opposed to a cancel) skips the workflow's cleanup — the
same way it can leave a source-snapshot hold behind — so it leaves the run's full staged
tree in place. A leaked staging directory is harmless (a future run stages under its own
run-id subdirectory and never collides with it) but it keeps consuming space on the pool
until removed. It is fully observable and clearable by hand:

1. **Identify runs no longer active** by comparing the subdirectories under
   `TAPE_STAGING_DIR` (each named for a Temporal run id) against Temporal — any
   subdirectory whose run id is not `Running` is safe to inspect.
2. **Remove the run's staged bulk data**, keeping the recovery artifacts if you still
   want them:

   ```
   find /mnt/bulk-pool-01/archive/.tape-staging/<run-id> -mindepth 1 \
     ! -name report.pdf ! -name recovery.iso ! -name disc-manifest.sha256 -delete
   ```

   or remove the run's directory entirely (recovery artifacts included) once you no
   longer need its report or master ISO:

   ```
   rm -rf /mnt/bulk-pool-01/archive/.tape-staging/<run-id>
   ```

Only reclaim a directory whose run has truly ended — removing an in-flight run's staging
tree destroys work the Write phase has not yet read onto tape.

---

## Releasing a tape stuck in a drive

`ltfs` sets the drive's medium-removal lock (PREVENT MEDIUM REMOVAL) when it mounts a tape
and clears it only when it exits cleanly. When the Write phase cannot unmount a tape it
kills `ltfs`, then detaches the dead FUSE mountpoint and clears the lock itself, so the
tape is still ejected (SPEC §4.3). A data worker that dies with a mount live (container
restart, host crash) cannot do that, so the changer then refuses to unload the tape (sense
ASC/ASCQ 53/02 or 53/03, "medium removal prevented"). That tape's write did not complete
and must be re-written anyway. The dead worker's FUSE mount lived only in its container's
mount namespace (the pool is bind-mounted `rslave`), so there is no stale mountpoint to
clean up on the host. Find the stuck drive's sg node with `lsscsi -g`, then on the storage
host, with no run using the drive:

```
sg_prevent --allow /dev/sgN          # N: the stuck drive's sg node
mtx -f /dev/sch0 unload SLOT DRIVE   # DRIVE: mtx's 0-based drive number
```

Clear the lock only once nothing is using the drive: it is what stops the changer pulling
a tape out from under a live mount.

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
