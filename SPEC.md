# tape-archiver — Specification

This is the authoritative technical reference for tape-archiver: its purpose, design
principles, architecture, run model, package contracts, and configuration. It is a
living document — update it in the same change that alters any behavior it describes.

This project is bootstrapped to mirror the conventions of
[`media-processor`](https://github.com/solidDoWant/media-processor): Go + Temporal,
Nix-reproducible builds, issue-driven development with Given/When/Then acceptance
criteria, and a testing-first philosophy.

## 1. Purpose

tape-archiver backs up ZFS snapshots to LTO tape for long-term, offline, cold storage.
A single "run" takes a configuration that fully describes the backup (what to archive,
how many copies, which library slots to use) and produces a set of physically
independent, self-describing tapes, plus a printed report and an optical recovery kit.

The design target is recoverability of tape contents in roughly **20 years**, by a
person who has only the tapes, the printed report, and the recovery disc — and no
access to this tool, this repository, or any online service.

## 2. Design principles

These are non-negotiable and drive every downstream decision. When a trade-off arises,
resolve it in favor of the principle, not convenience.

1. **~20-year recoverability.** Everything written to tape must be recoverable two
   decades later. In practice: only open, widely-implemented standards and formats
   (`tar`, `age`, PAR2, LTFS, ISO 9660); error correction on the media; and the exact
   recovery tooling (static binaries + source + written procedure) shipped on the
   recovery disc alongside the data.
2. **Minimize tape wear.** Tapes tolerate a limited number of passes. The dominant
   wear mechanism is "shoe-shining" (back-hitch / stop-start-rewind) caused by feeding
   the drive below its speed-matching floor. The pipeline must feed each drive fast
   enough to stream continuously, and the project must *prove* this rate by benchmark
   in production-like conditions.
3. **Plan for media degradation and failure.** Tapes degrade and fail. Mitigate with
   multiple copies on multiple physical tapes, per-archive PAR2 error correction, and
   bounded blast radius so a single bad physical segment loses at most one recoverable
   unit.
4. **Everything is tested.** If it is not tested, it does not work. Unit-test logic,
   integration-test the full tape path against a virtual library, and benchmark the
   streaming rate against real hardware before trusting it.

## 3. Target environment

The production environment is a specific personal homelab. The tool is written against
it directly rather than for general portability.

- **Storage host:** `ubuntu-storage-host-01` (Ubuntu). Reachable via Teleport
  (`tsh ssh root@ubuntu-storage-host-01`). The tape library and drives are physically
  attached here. Has `mt`, `zfs`, `lsscsi`, `sg_map`, `sg3-utils`. Does **not**
  yet have `ltfs`/`mkltfs`, `age`, or `par2` — these are provided by the worker
  container image (see §4).
- **Tape library:** SCSI media changer at `/dev/sch0` — **2 drives, 47 storage slots,
  3 import/export (I/O) slots**. Drives present as `/dev/st0`+`/dev/nst0` and
  `/dev/st1`+`/dev/nst1` (use the non-rewinding `nst` nodes). Drives are **LTO-6**
  (2.5 TB native, ~160 MB/s native, speed-matching floor ~50 MB/s).
- **Pool:** `bulk-pool-01` (ZFS, ~58 TB, mounted on the host at `/mnt/bulk-pool-01`,
  and on the dev box via virtiofs at the same path). Datasets include
  `bulk-pool-01/archive`, `bulk-pool-01/media`, and the k8s-managed
  `bulk-pool-01/k8s/democratic-csi/nfs/pvcs/pvc-*`.
- **Kubernetes storage:** volumes are provisioned by **democratic-csi over NFS** as
  ZFS **filesystem** datasets (not zvols). A `VolumeSnapshot`'s bound
  `VolumeSnapshotContent` carries a CSI `snapshotHandle` of the form
  `pvc-<uuid>@snapshot-<uuid>`, expressed **relative to democratic-csi's
  `datasetParentName`** (`bulk-pool-01/k8s/democratic-csi/nfs/pvcs`), which is prepended
  to rebuild the absolute ZFS snapshot path. democratic-csi stamps datasets and
  snapshots with `democratic-csi:` ZFS user properties — notably `managed_resource`
  (the "owned by democratic-csi" marker) and `csi_volume_name` (the PV name
  `pvc-<uuid>`). PVC name and namespace are **not** stamped as ZFS properties. The
  `managed_resource` marker is the signal used to confirm a resolved snapshot is
  democratic-csi-owned before any data is staged; provenance in the report is taken
  from the k8s objects, not from ZFS properties.
- **Kubernetes cluster:** hosts the Temporal control-plane worker and the source
  `VolumeSnapshot`/snapshot-group resources. The storage host is outside the cluster.

## 4. Architecture

### 4.1 Orchestration: Temporal, two task queues

Temporal orchestrates each run. There are two workers on two task queues, split by
where the data lives:

- **Control worker (`control` queue) — runs in Kubernetes (optionally on demand).**
  Lightweight, no bulk data. Resolves the run config against the k8s API (snapshot
  discovery/validation), drives the workflow, and bin-packs the plan. By default a
  fixed-replica `Deployment`; the Helm chart can optionally deploy it as a KEDA `ScaledJob`
  that scales to zero between runs and is woken `0 → 1` by a `control`-queue backlog, exiting
  again after an idle window (`WORKER_IDLE_EXIT_AFTER`). See `docs/control-worker-helm.md`.
- **Data worker (`data` queue) — runs as a container on `ubuntu-storage-host-01`.**
  Performs all bulk-data activities where the bytes already are, so they never cross
  the network: `tar`, `age`, PAR2 slicing, checksums, LTFS format/mount/write, and
  library moves. It also builds the PDF report — and, when optical burning is enabled,
  the recovery ISO — and delivers the report to Discord: the report/ISO inputs — the
  staged files, the recovery binaries staged into the ISO, the pinned tool versions, and
  the captured LTFS indexes — all live here, and building from here keeps the tens-of-MB
  ISO off the Temporal payload path and out of the control image.

The data worker is a **Nix-built OCI image** (`streamLayeredImage`, per media-processor)
run by systemd-managed Docker on the host. The image bundles pinned tooling — `ltfs`,
`age` (>= 1.3.1), `par2` (par2cmdline-turbo), `zstd`, `mt-st`, `sg3-utils`,
`lsscsi` — at the **same versions** shipped on the recovery disc, so backup tooling and
recovery
tooling come from one reproducible source. Both workers report to the same Temporal
cluster, so the entire job tree is observable in the Temporal UI.

Container runtime requirements: tape/changer device passthrough (`/dev/nst0`,
`/dev/nst1`, `/dev/sch0`, and the drives' `/dev/sg*` nodes), `/dev/fuse` +
`CAP_SYS_ADMIN` (LTFS is FUSE-based), and a bind mount of `/mnt/bulk-pool-01` with
shared/rslave mount propagation so `.zfs/snapshot/<snap>/` and the staging directory
are visible. The worker stages into a plain subdirectory of an existing dataset (e.g.
`bulk-pool-01/archive/.tape-staging/`) to avoid needing `zfs create`/`/dev/zfs`.

### 4.2 Run model: config-driven, full, independent, stateless

- **The run config is the source of truth.** A run is fully defined by a configuration
  supplied as input — either a file or the Temporal workflow payload. See §5.
- **Every run is a 100% full backup** of everything the config names. There is **no
  deduplication between runs** and **no incremental backup**.
- **No online catalog, no cross-run state.** The tool keeps no database of what has
  been archived. The system of record is the physical media plus the printed report
  and the recovery ISO. Restore depends on zero online services. This forbids state that
  *survives a run*; it does not forbid the durable *single-run* execution state Temporal
  keeps within one run. Operator-in-the-loop pauses (the Eject I/O-station pause, §4.3
  phase 8, and the Load/Write-failure pause, §4.3 phases 6–8) hold their resume state in
  the run's own workflow event history and discard it when the run ends — they are not a
  catalog and not cross-run state.
- **Tape inventory is resolved live, per run.** At startup the tool reads the library
  with SCSI `READ ELEMENT STATUS`; the config declares which storage elements hold usable blank
  tapes. Written tapes are exported to the I/O station at the end of the run; the
  operator reloads blanks before the next run. Automated reloading of fresh blank
  tapes into storage slots *between* runs is a non-goal — that is the operator's job.
  Clearing the I/O station *within* a run whose written tapes exceed I/O-slot capacity
  is not: the Eject phase pauses and prompts the operator to remove the exported tapes,
  then resumes (§4.3 phase 8).

### 4.3 Backup pipeline (workflow phases)

A run proceeds through these phases. Phases up to and including Verify produce nothing
on tape; **no data is written to tape until the complete contents of every tape are
staged and verified on disk** — eliminating any computation during the write window.

1. **Resolve.** Expand the config into a concrete work list: resolve k8s
   `VolumeSnapshot`/snapshot-group references to ZFS snapshots (cross-checked against
   the democratic-csi `managed_resource` property to confirm ownership before staging),
   and validate raw ZFS snapshot/dataset paths. Run a cheap
   feasibility pre-check from `zfs` properties (`logicalreferenced`, inflated by a small
   `tar` overhead and the configured PAR2 %) purely to reject any single archive that
   cannot fit on one tape *before* doing real work. The same pre-check also rejects a
   `sliceSizeBytes` so small for the resolved source size that the run's total slice
   count would grow an activity payload past Temporal's ~2 MB limit, naming the field
   and a suggested minimum. This is an estimate, not the plan.
2. **Prepare.** For each archive: `tar` the snapshot contents → optional `zstd`
   compression → `age`-encrypt → split into fixed-size slices → compute SHA-256
   checksums. All output is staged to disk and its exact size measured.
3. **Pack.** Bin-pack the prepared archives onto tapes by their *measured* size
   (≤ tape capacity, accounting for PAR2 and LTFS overhead), replicated across N copies
   (N = configured copy count). The copy count is **not** bounded by the drive count:
   the Write phase writes at most one drive-set (≤ number of drives) of physical tapes
   in parallel at a time and iterates over both logical tapes and copies (steps 6–8), so
   a run may span any number of logical tapes and any number of copies. Plan against
   2.5 TB native capacity with LTO hardware compression disabled — `age` output is
   incompressible, so drive compression only adds variability.
4. **Generate PAR2.** For each archive, generate its per-archive PAR2 recovery set.
   Fixed-percentage mode sizes it directly; fill-to-capacity mode raises the percentage
   uniformly to consume each tape's remaining space down to a configured floor. The
   PAR2 block size is computed from tape capacity to stay within PAR2's 32,768 recovery
   block limit. Output is staged and checksummed.
5. **Verify.** Re-read all staged files and verify checksums; confirm each planned
   tape's complete tree is present and within capacity. A run cannot proceed to write
   on any verification failure.
   Steps 6–8 run per **drive-set** — at most `len(Drives)` physical tapes, one per drive.
   When a run needs more physical tapes (logical tapes × copies) than the library has
   drives, they are written as a sequence of drive-sets: each set is loaded, written, and
   ejected — freeing the drives — before the next set is loaded. Processing one set at a
   time bounds the tapes loaded and read concurrently to the drive count, protecting the
   write-rate floor (§14). A drive-set groups adjacent (logical tape, copy) pairs, so the
   copies of one logical tape tend to share a set and read a single staged tree. A Load or
   Write failure within a set does not fail the whole run: the set's successfully written
   tapes are ejected and recorded, the failed tapes are ejected too (freeing their drives
   and emptying their blank slots), and the run **pauses for operator approval** — it alerts
   the operator (§11) and waits on a resume or abort signal. On resume it re-drives only the
   failed (logical tape, copy) pairs onto fresh blanks in the same slots — never
   re-formatting a tape already written (step 6's blank-check gates that), so the blast
   radius is the failed tapes, not the whole set or the whole run. On abort — or if the
   operator does not respond within `library.writeFailureWaitTimeoutSeconds` (default 12 h)
   — the run ends in that defined paused state and is reported. No later set is loaded until
   the failed set completes or the run ends. This mid-run pause is durable *single-run*
   workflow state (the plan and which tapes are written vs. pending live in the run's
   Temporal event history), not the cross-run state or online catalog §4.2 forbids — the
   same operator-in-the-loop shape as the Eject pause below. (Reloading fresh blank tapes
   into storage slots between runs remains a non-goal, §4.2. Clearing the I/O station
   *within* a run when written tapes exceed I/O capacity is handled by the
   operator-in-the-loop Eject pause below.)
6. **Load.** Move the drive-set's blank tapes from their storage slots into the drives
   (SCSI `MOVE MEDIUM`), and confirm each loaded tape is blank/empty before formatting — a run must
   never silently overwrite existing data. The blank check always runs; when a loaded tape is
   **not** blank the run fails before any format/write, *unless* the run config sets
   `library.allowNonBlankTapes`, in which case the run instead logs a prominent warning naming the
   tape's barcode and slot, proceeds to overwrite it, and records the overwrite in the run report.
   The override changes only the non-blank outcome — never detection — and never silences it.
7. **Write.** Before any tape in the set is formatted or mounted, validate the set's
   barcodes: every loaded tape must carry a **non-empty, set-unique** barcode. The per-tape
   LTFS mountpoint and work directory are keyed on the barcode, so an empty barcode (an
   unlabeled blank — a barcode is only read when the SCSI PVOLTAG bit is set) or a
   collision would make two parallel writes share one mountpoint. The run fails naming the
   offending tapes before any LTFS volume is mounted or written; reloading the same tapes
   cannot clear the condition, so this is a fatal fault, not an operator pause.
   `mkltfs` each tape in the set (setting the LTFS volume name to the tape's
   barcode), mount LTFS **with index sync deferred to unmount** (`-o sync_type=unmount`),
   and stream the staged tree to tape. The set's tapes write to their drives in parallel.
   Writing is a pure sequential disk→tape copy whose sustained rate is monitored. The LTFS
   index is therefore written **once**, at unmount during Eject, rather than periodically
   during the write — see §14. A per-tape checksum/manifest file is written last; the
   LTFS index is read back after unmount and captured for the ISO.
8. **Eject.** Unmount/unload each written tape in the set and transfer it to an I/O
   station slot for physical removal, freeing its drive for the next set. Each tape is
   unloaded from its drive to its source storage slot *before* the I/O transfer, so a
   tape is never stranded in a drive. When a run writes more tapes than the library has
   I/O slots, the station fills: the phase then becomes **operator-in-the-loop** — it
   alerts the operator which tapes to remove (§11) and pauses, leaving every written tape
   in an I/O or storage slot. It resumes automatically on libraries that report the
   import/export ACCESS bit (once the station is cleared and closed), or on the explicit
   `operatorResume` signal (`tapectl resume`) otherwise, then exports the
   remaining tapes into the freed slots. If the operator does not respond within
   `library.ioWaitTimeoutSeconds` (default 12h), the run fails in that defined state and is
   reported.
9. **Report.** Build the PDF report (§9), and — only when `delivery.opticalBurn` is
   configured — the recovery ISO (§10) as the mountable image the Burn phase consumes.
10. **Burn (optional).** When `delivery.opticalBurn` is configured, burn the recovery
    disc from the staged uncompressed ISO (§10) and read each disc back to verify it.
    Copies are burned in successive **burn-sets** of at most `len(drives)` discs;
    because there is no optical autoloader, the run pauses between sets for the
    operator to load fresh blanks and resume, and pauses on any burn/verify failure or
    refused non-blank disc — the operator-in-the-loop shape of the tape Write/Eject
    pauses, on the same `operatorResume`/`operatorAbort` signals, bounded by
    `delivery.opticalBurn.burnWaitTimeoutSeconds` (default 12 h). The Report phase runs
    before Burn, so the report inside the burned ISO predates the burn and cannot record
    it; after Burn the **delivered** `report.pdf` is re-rendered from the full run state
    to record the discs (and any overwrite). Disabled by default: a run with no
    `opticalBurn` section completes exactly as without this phase.
11. **Deliver.** Send the PDF report to Discord via webhook (§11).

## 5. Run configuration

The run config fully and explicitly defines a backup. It is supplied as a file or as
the Temporal workflow payload. A JSON Schema is generated from the Go types
(`make generate-schema`, committed under `schemas/`) and documented in `docs/`.

The config defines, at minimum:

- **Sources** — the things to archive, as any mix of:
  - **k8s snapshots:** `VolumeSnapshot` / snapshot-group references (by name/namespace
    and/or label selector across namespaces). A snapshot **group is archived as a
    single tar** (one subdirectory per member volume), giving cross-volume consistency.
  - **Raw ZFS paths:** explicit ZFS snapshot or dataset paths on the pool not visible
    to k8s (e.g. `bulk-pool-01/archive`, `bulk-pool-01/media`).
- **Copies (N)** — number of identical physical tape copies to produce (default 2, the
  drive count). Up to `len(Drives)` copies write in parallel; a copy count exceeding the
  drive count is written across successive drive-sets (§4.3 steps 6–8). The library must
  hold one blank tape per physical tape written (logical tapes × copies).
- **Library** — device targets (real `/dev/sch0` + `/dev/nstX`, or a virtual library
  for dry-run, §12) and the list of storage slots holding usable blank tapes. An optional
  `allowNonBlankTapes` flag (default off) opts the run out of the non-blank refusal so it may
  deliberately overwrite used tapes (§4.3 step 6).
- **Redundancy** — PAR2 policy: a target redundancy percentage, or **fill-to-capacity**
  (size data first, then expand PAR2 to consume remaining tape down to a configured
  floor). Slice size is configurable.
- **Compression** — optional `zstd` before encryption, configurable per source,
  default on. Already-compressed sources (e.g. `media`) gain little but are unharmed.
- **Encryption** — the age recipient(s) (`age1pq1…`) to encrypt to.
- **Delivery** — the Discord webhook target (report delivery) and optical-burn options.

## 6. On-tape layout and formats

All formats are open and widely implemented, for 20-year recoverability.

- **Container:** **LTFS** (LinearTape-Open `ltfs`) presents each tape as a
  self-describing filesystem with a readable index. Files are stored as regular files;
  a copy of the LTFS index is also captured to the recovery ISO in case the on-tape
  index is damaged.
- **Tape identity:** the library-read **barcode (SCSI volume tag) is the
  canonical physical ID**. `mkltfs` sets the LTFS volume name to the barcode; the
  per-tape manifest and the report reference tapes by barcode. (Production tapes are
  barcode-labeled and read by the library.)
- **Directory layout within LTFS:**
  - `archives/NNN-<label>/` — one directory per archive, where `NNN` is the zero-padded
    source index and `<label>` a short, sanitized descriptive name for the source
    (e.g. `archives/000-photos/`, `archives/001-plex-group-snap/`). The `NNN` prefix
    orders the directories and keeps them unique even when two sources share a label;
    `<label>` is the source's optional `label` override, or a name derived from its
    identity (a raw ZFS source's dataset last component, a named k8s resource's name,
    or a label selector), sanitized to `[a-z0-9._-]` and bounded in length. The slice
    and PAR2 basenames inside are unchanged (`archive.NNN`, `archive*.par2`), so the
    recovery globs still match. The `NNN` slice suffix is zero-padded to a uniform
    width per archive — three digits by default, widened to fit the slice count (e.g.
    four digits once an archive exceeds 1000 slices) — so lexical filename order always
    equals numeric slice order and the recovery glob (`archive.[0-9]*`) reassembles them
    correctly regardless of count. Each directory contains: the fixed-size
    `age`-encrypted, optionally `zstd`-compressed `tar` slice files; and the PAR2
    recovery set covering those slices.
  - `manifest.json` — top-level checksum manifest at the LTFS root, written **last**
    (after all archive directories are fully written). Its presence signals that the tape
    was completely written; a tape without `manifest.json` was not finished by the run and
    must be discarded.
- **`manifest.json` contents:** barcode, tape index, copy index, and for each archive: its
  source index, slice file paths (relative to the LTFS root), SHA-256 checksums, and PAR2
  file paths with checksums. Checksums come from the Prepare/GeneratePAR2 phases — no
  hashing occurs during the write window (SPEC §14).
- **Per archive (group or volume), the on-tape files are:**
  - encrypted, sliced data: fixed-size slices of the `age`-encrypted, optionally
    `zstd`-compressed `tar` stream;
  - a PAR2 recovery set covering those slices.
- **Per tape:** the top-level `manifest.json` covering all files on the tape, plus the
  LTFS index (captured by `FinalizeTape` at unmount and also on the tape itself).
- **Source format:** `tar` of the snapshot's `.zfs/snapshot/<snap>/` contents.
  Filesystem-level `tar` (not `zfs send`) is chosen deliberately: it is the most
  portable, longest-lived, application-agnostic representation, restorable file by file
  without ZFS. Optional `zstd` compression (open, RFC 8878) is applied before
  encryption; its binary and source are bundled on the recovery disc.
  - **What the archive captures:** regular files, directories, and symlinks, with
    their permission bits, ownership (uid/gid), and modification time. Hardlinked
    regular files are stored once and reproduced as `tar` hardlink entries, so a
    logically deduplicated dataset (e.g. the *arr-managed media dataset) is not written
    twice. Sparse files are stored in **GNU sparse 1.0** (PAX `GNU.sparse.*`) form —
    their holes are not written out and are restored as zeros — a construct the shipped
    static GNU `tar` decodes natively.
  - **What the archive does NOT capture:** extended attributes (`user.*`, `security.*`),
    POSIX ACLs, and file capabilities (`security.capability`, e.g. `setcap` binaries)
    are **not** preserved; a `tar`-level restore reproduces file contents and the mode
    bits above but not this metadata. File types that a portable file-by-file `tar`
    cannot represent and that carry no recoverable data — unix sockets, device nodes,
    and named pipes (FIFOs) — are **skipped with a warning** rather than failing the
    run, so a stale socket in an application datadir (e.g. `mysql.sock`) does not abort
    a backup.

## 7. Encryption and key management

- **Tool:** mainline `age` >= **1.3.1**, using its **native post-quantum recipients**
  (HPKE with hybrid **ML-KEM-768**; never weaker than X25519 alone). Keys are generated
  with `age-keygen -pq`; recipients are `age1pq1…`, identities `AGE-SECRET-KEY-PQ-1…`.
  No plugin is required. The age binary, its source, and the C2SP format spec are
  bundled on the recovery ISO.
- **Key escrow (operator decision):** the **private identity is included** in the
  printed report and on the recovery ISO, so the holder of those artifacts can always
  decrypt. Consequence to document plainly: the report delivered to Discord (and the
  recovery ISO on its burned disc) therefore contains the decryption secret and must be
  handled accordingly. (Treated as acceptable for this personal cold-storage threat
  model.) The identity is supplied in
  the run config (`encryption.identity`); it is **never used to encrypt** (that uses
  `encryption.recipients` only), and the Report phase fails the run if it is absent or
  if its derived public key is not one of the configured recipients — so the escrowed
  key is always one that can actually decrypt the archives.

## 8. Redundancy model

Two complementary, independent mechanisms — the report explains both so a future
recoverer understands what protects against what:

- **PAR2 (within a tape):** protects against *localized* media decay — bad blocks, a
  damaged segment. Recoverable up to the configured redundancy. Block size is computed
  from tape capacity so as not to exceed PAR2's hard limit of 32,768 recovery blocks.
  Using otherwise-wasted tape capacity for additional parity is encouraged
  (fill-to-capacity mode).
- **N copies (across tapes):** protects against *whole-tape* loss — a snapped,
  demagnetized, or lost tape, which PAR2 cannot recover at any percentage.

Bounded blast radius: because each archive is sliced and independently recoverable, a
single bad physical region damages at most one slice, recoverable via PAR2 on the same
tape or via the redundant copy on another tape.

Recovery-model note (drives the compression decision): when damage stays **within**
PAR2's correction capacity, PAR2 reconstructs the exact original bytes and the archive
decrypts and unpacks fully. When damage **exceeds** PAR2 capacity, `age`'s streaming
AEAD (independently-authenticated ~64 KiB chunks, decrypted in order) aborts at the
first uncorrectable chunk, so the archive is truncated from that point — regardless of
whether it was `zstd`-compressed. Blast radius within an archive is therefore governed
by PAR2 redundancy and the N copies, not by the compress/don't-compress choice. This is
the reason compression is enabled by default, and an argument for sizing PAR2 generously
(fill-to-capacity) so correction capacity is rarely exceeded.

## 9. Report (PDF)

Produced by the data worker (§4.1: its inputs — staged files, pinned tool versions,
recovery binaries, captured LTFS indexes — all live there). Contains, at minimum: run
id and date; full contents
manifest (archives, member volumes, source snapshots, sizes, SHA-256 checksums);
which physical tape(s) (by barcode/label) hold what (annotating any tape written over a
non-blank tape under `library.allowNonBlankTapes`, §4.3 step 6); per-tape **write health** (§14);
how the tapes were built (tool
version, `age`/`par2`/`ltfs` versions, slice size, PAR2 redundancy, drive/library
identifiers); the **age private identity**; and the recovery procedure. Intended to be
printed and laminated as the durable offline index for the run.

## 10. Recovery ISO (optical)

An ISO 9660 image that is the self-contained recovery kit. It is built **only when
optical burning is enabled** (`delivery.opticalBurn`), as the mountable image the Burn
phase burns to each disc — the burned disc is the ISO's durable home, so a run without
burning produces no ISO. Contains: the PDF report; the full SHA-256 manifest; a backup
copy of each tape's LTFS index; and the **recovery tooling** — static
`age`/`par2`/`zstd`/`tar` (and LTFS read instructions) plus their source and a written,
step-by-step recovery procedure — so the tapes can be read, repaired, decrypted,
decompressed, and unpacked with only the disc and the tapes.
The static tooling and its source are produced by `nix/recovery-binaries.nix` (flake
output `recoveryBinaries`) from the same pinned nixpkgs the data-worker image uses, so
the disc and the write path run identical tool versions (§2, §4.1).

**Target media: M-DISC DVD.** Its inorganic recording layer is ISO/IEC 10995-tested and
NIST-listed for 100+ year archival life, and — unlike recordable Blu-ray, whose media
and drive production are being discontinued (Sony exited recordable BD in Feb 2025 with
"no successor") — it is readable in the large, long-lived installed base of DVD drives.
The ISO is tens of MB, so DVD capacity is ample. Burn **at least two copies** and verify
each by reading back and comparing against the manifest. Optical is one redundancy layer,
not a hard dependency: the laminated report independently carries the key, procedure, and
manifest, and every tape carries its own LTFS index and checksums.

**Burning is an optional in-workflow phase** (§4.3 phase 10), enabled by
`delivery.opticalBurn`; it is not only a manual step. When configured, the Burn phase
burns the staged uncompressed ISO to each disc and reads it back to verify against the
disc-content manifest, in burn-sets of at most `len(drives)` discs, pausing for the
operator between sets (a manual disc swap — there is no optical autoloader) and on any
failure. By default a non-blank disc is refused and the run pauses rather than
overwriting it; `delivery.opticalBurn.allowNonBlankDiscs` opts into reclaiming a used
disc, but **only rewritable media (DVD±RW / BD-RE) can be reclaimed** — write-once media
(DVD-R, **M-DISC**, CD-R, BD-R) cannot be erased, so a non-blank write-once disc always
pauses for the operator regardless of the opt-in. The opt-in also **never reclaims a disc
this run itself already burned and verified**: a non-blank rewritable disc found in a burner
that has already produced a verified copy this run is that copy still loaded (a duplicated or
skipped pause, or a forgotten disc swap), not a prior-run leftover, so the run pauses for a
fresh blank rather than blanking it — otherwise it would silently destroy a copy the report
counts, losing configured redundancy (§2 principle 3). Any deliberate overwrite (only ever a
genuine prior-run disc) is recorded in the delivered report. Leaving `opticalBurn` unset keeps
burning a manual operator step.
Periodic re-burn/refresh remains a documented maintenance task.

## 11. Notifications (Discord)

There are two distinct Discord notification paths:

**Success delivery (per-run, configured in the run config).** At the end of a
successful run the data worker delivers the PDF report to the Discord webhook named in
the run config (§5 Delivery) — from the data worker, where it was built (§4.1). The
recovery ISO travels on its burned disc (§10); the report is the single uploaded
artifact, which keeps the upload comfortably within the webhook's ~25 MB limit.

**Failure alert (operational, configured via env var).** A separate failure webhook,
configured on the worker via the `DISCORD_FAILURE_WEBHOOK_URL` environment variable
(parsed in `internal/envvar`), alerts on any run failure — mirroring media-processor.
It is intentionally infrastructure-level rather than per-run config so that a run which
fails before or while parsing its config can still alert. When set, a workflow failure
(or cancellation) posts a concise message: run id, the failing phase/activity, the error
summary, and any partial context (e.g. tapes already written and needing manual
handling). When unset, failure alerting is disabled (a no-op). Delivery of the alert
must never mask the original error — a webhook failure is logged, not raised.

This is implemented with Temporal's standard on-failure pattern: a deferred handler in
the backup workflow runs the alert activity on a `workflow.NewDisconnectedContext` so it
fires even when the workflow is cancelled. The alert uses the same `pkg/webhook` client
as success delivery.

**Operator-pause alert (operational, same failure webhook).** When the Eject phase fills
the I/O station and pauses for the operator (§4.3 phase 8), it posts a message on the same
`DISCORD_FAILURE_WEBHOOK_URL` naming the run, the tapes now in the I/O station ready for
removal, and how many still await a free slot. Like the failure alert it is best-effort —
a delivery failure is logged, never raised — so a webhook outage never aborts a run that
is only waiting for the operator. The operator clears the station and, on libraries that
do not report the import/export access bit, runs `tapectl resume` to continue.

**Write-path pause alert (operational, same failure webhook).** When a Load or Write fails
for one drive-set the tape path pauses for the operator (§4.3 phases 6–8) rather than
failing the whole run. It posts a message on the same `DISCORD_FAILURE_WEBHOOK_URL` naming
the run, the failing phase, the tapes affected by the failure, the storage slots to restock
with fresh blank tapes, and the error summary, and it tells the operator the exact
`tapectl resume` / `tapectl abort` commands. Like the other alerts it is
best-effort — a delivery failure is logged, never raised. The operator either swaps the
affected tapes for fresh blanks and resumes, or aborts; if neither happens within
`library.writeFailureWaitTimeoutSeconds` (default 12 h) the run fails in that defined
paused state and is reported.

**Resume signals are matched to the pause that is waiting.** Every operator pause — the
Eject I/O-station pause, the write-path pause, and the optical-burn pause — resumes only on
a resume signal sent *after* it began (i.e. after the operator has seen its alert). A resume
already buffered when a pause begins is stale — a double `tapectl resume`, or one that raced
an auto-resume (the Eject poll can resume on the access bit while a signal is still in
flight) — and is discarded at the pause's entry, so it can never instantly satisfy a later
pause. Without this, a surplus resume could skip a between-burn-set disc-swap pause and blank
a just-verified recovery disc. A buffered *abort* is never discarded: aborting is always a
safe, reported, no-further-data outcome.

## 12. Dry-run and the virtual library

Dry-run is a first-class mode. Device targets are configurable, so a dry-run points the
worker at a **virtual tape library (`mhvtl`)** instead of `/dev/sch0` + `/dev/nstX`.
The same code path exercises virtual hardware end to end (load, `mkltfs`, write, eject).
This mechanism is shared with the integration test suite. Note: `mhvtl` virtual tapes
consume real scratch disk, so full-capacity dry-runs need real space; scaled-down test
data is used in CI.

## 13. Testing strategy

Testing is a first-class requirement (principle 4). Layered, mirroring media-processor's
build-tag-gated tiers:

- **Unit** (default, `-race`): pure logic — planning/bin-packing, config parsing, slice
  sizing, PAR2 block-size computation, SCSI element-status decoding, parsing of `mt`/`sg_logs` output.
- **Integration** (`//go:build integration`): the full tape path against an **`mhvtl`**
  virtual library and a dev Temporal — load, format, write, verify, eject. `pkg/zfs`
  tests run against an ephemeral, file-backed **ZFS pool** (`make zpool-up`) rather than
  the production pool, so they are deterministic and never touch live data. Skips when
  required env/devices are absent.
- **Real-hardware + benchmark** (build-tag gated, env-skipped): exercises the physical
  library and, critically, **measures sustained write MB/s and scrapes drive log pages
  (`sg_logs`) for back-hitch / TapeAlert flags** to prove the anti-shoe-shining rate
  (principle 2) before the tool is trusted in production.
- **End-to-end** (`//go:build e2e`): the whole workflow including report/ISO/delivery
  against virtual hardware.

Acceptance criteria are Given/When/Then describing observable behavior; tests are never
modified to pass — the code is fixed instead.

## 14. Performance requirements

- The write phase must feed each drive above its **speed-matching floor (~50 MB/s for
  LTO-6)** continuously, with no back-hitch. This is enforced by design (everything is
  staged before writing) and **measured on every run**: after each tape's write window
  closes, the run records that tape's sustained write throughput (staged bytes ÷
  write-window elapsed), its reposition/back-hitch count (SCSI log page `0x24`), and any
  TapeAlert flags (log page `0x2e`) in the PDF report (§9) and as Prometheus gauges, and
  flags any tape that streamed below the floor or back-hitched. The floor is the tape
  **generation's** speed-matching floor (the write format governs the drive's
  speed-matching range), derived from the configured native capacity — LTO-5 40, LTO-6
  ~50, LTO-7 100, LTO-8 112, LTO-9 180 MB/s (nominal published minimum speed-matching
  rates); a capacity that maps to no known generation reports throughput without a
  below-floor verdict rather than judging against a guessed value. This is
  **observational only** — it never fails or gates a run — so principle 2 can be
  evaluated against the real workload before any gating is considered.
- **Bound write concurrency to the drive count.** The Write phase writes at most one
  drive-set (≤ `len(Drives)`) of physical tapes at a time (§4.3 steps 6–8); a run needing
  more physical tapes iterates over drive-sets rather than reading more streams at once.
  All tapes in a set read from the bulk pool concurrently — the same staged tree (for
  copies of one logical tape) or different trees — so capping the in-flight set at the
  drive count keeps disk read bandwidth predictable and protects the speed-matching floor.
  Copies of a logical tape are grouped into a set where possible: the ZFS ARC and
  sequential read-ahead coalesce their in-lockstep reads to near-1× physical disk reads,
  and a lagging drive re-reads from its own page-cache window. Re-reading a logical tape's
  staged tree across successive copy-sets is expected and acceptable.
- The prepare phase (`tar`/`age`/PAR2) must, in aggregate, keep ahead of the planned
  write throughput; PAR2 uses the multithreaded par2cmdline-turbo. Because prepare and
  write are decoupled by staging, prepare throughput affects total run time but never
  the streaming guarantee.
- **Defer the LTFS index sync to unmount** (`-o sync_type=unmount`), so the index is
  written a single time, just before Eject, instead of periodically during the write.
  Each periodic index sync forces the drive to reposition and write to the index
  partition mid-stream — a back-hitch and an extra write pass that this workflow has no
  reason to incur, since a run writes each tape once and ejects it. This directly serves
  principle 2 (minimize tape wear). Accepted trade-off: if the host crashes or loses
  power before unmount, data already written to tape is not yet referenced by an index
  and would need LTFS deep/roll-back recovery — acceptable here because every run is a
  full, re-runnable backup, there are N copies, and the index is captured to the ISO at
  unmount. No strong downside identified for this access pattern.

## 15. Proposed code layout

Mirrors media-processor (`cmd/` + `internal/` + `pkg/` + `workflows/`). Subject to
refinement as issues are implemented.

- `cmd/worker` — Temporal worker; role (control | data) selects task queue + registered
  activities.
- `cmd/tapectl` — CLI to submit a run config to Temporal, trigger dry-runs, inspect.
- `cmd/gen-config-schema` — emits the committed JSON schema for the run config.
- `pkg/` — one concern per package: `tape` (changer via SG_IO, `mt`), `ltfs`, `agewrap`, `par2`,
  `archive` (tar), `zfs`, `k8ssnap` (VolumeSnapshot discovery), `report` (PDF),
  `recoverykit` (ISO), `webhook` (Discord), `checksum`, `logging`, `metrics`,
  `temporalclient`.
- `internal/config` — run-config types and env parsing.
- `workflows/backup/` — the backup workflow and activities, split by concern
  (`resolve.go`, `plan.go`, `prepare.go`, `verify.go`, `write.go`, `library.go`,
  `report.go`, `deliver.go`) with co-located tests.
- `schemas/` — generated config schema. `deploy/charts/` — Helm chart for the control
  worker. `docs/` — operator docs. `e2e/` — end-to-end tests.
- `nix/` — build derivations: `ltfs.nix`, the `mhvtl` userspace/kernel modules, and
  `recovery-binaries.nix` (the static `age`/`par2`/`zstd`/`tar` set plus source staged
  for the recovery disc, exposed as the flake output `recoveryBinaries`; §10).

## 16. Resolved decisions and remaining open items

The following were settled during bootstrap (see §3, §4.3, §6, §8, §10):

- **Tape identity** — library barcode (SCSI volume tag) is the canonical ID; LTFS volume
  name set to barcode; report and per-tape manifest reference by barcode.
- **Compression** — `zstd` before encryption, uniform default-on with a per-source
  override; safe because `age` + PAR2 already bound blast radius (§8).
- **k8s resolution ownership** — control worker resolves (label selectors →
  `snapshotHandle` → ZFS name); data worker verifies the resolved snapshot exists and is
  democratic-csi-managed (`managed_resource`); raw ZFS sources are data-worker-only.
- **Recovery disc** — M-DISC DVD, ≥2 copies, verify after burn; optical is a redundancy
  layer, not a hard dependency.
- **Sizing** — cheap `zfs logicalreferenced` feasibility pre-check; authoritative
  bin-pack on *measured* staged sizes; pipeline reordered to prepare → pack → PAR2 →
  verify so fill-to-capacity PAR2 is well-defined.
- **Feasibility overhead factor** — the Resolve pre-check estimates an archive's on-tape
  size as `logicalreferenced × overhead × (1 + PAR2 fraction)` and rejects any single
  archive exceeding the configured tape capacity (`library.tapeCapacityBytes`, e.g. an
  LTO-6 tape's 2.5 TB native). The overhead factor covers
  `tar` headers/padding and `age` STREAM framing; `zstd` is assumed to yield no reduction
  (incompressible worst case) so the estimate never runs low. It defaults to **1.05** (5%,
  a generous margin for many-small-file datasets; value chosen in issue #17) and is tunable
  per run via the `feasibilityOverhead` config field. The PAR2 fraction is the target percentage, or the
  floor in fill-to-capacity mode. This is only the pre-check estimate; the authoritative
  size is the measured staged size from Prepare.
- **LTFS implementation** — the reference open-source **LinearTape-Open `ltfs`**
  (IBM-maintained, multi-vendor, Apache-2.0), pinned at **v2.4.8.4** (selected in issue #12)
  and built from a Nix
  derivation (`nix/ltfs.nix`); the worker image and recovery disc ship the same version.
  The vendor-locked `hpe-ltfs` was rejected: it refuses non-HPE drives (so it cannot be
  tested against the mhvtl IBM-emulated drive), and the production LTO-6 drives are IBM.
  The reference `ltfs` drives the tape through its **`sg` backend (the `/dev/sg*` node)**,
  not the `nst` node — both are already passed through to the data worker (§4.1).

The last two open items are now resolved (issue #21), both as documented operator
procedures in `docs/maintenance.md`:

- **Tape barcode format convention** — `TA<NNNN>L<gen>` (project prefix `TA`, a
  zero-padded sequence, then the standard LTO media-generation suffix, e.g.
  `TA0001L6` for LTO-6). The prefix and sequence are cosmetic: the canonical
  physical ID is whatever the library reads as the SCSI volume tag, and `mkltfs`
  sets the LTFS volume name to that barcode (§6). Documented in
  `docs/maintenance.md`.
- **Recovery-disc re-burn / refresh cadence** — verify each burned disc's
  readability against `manifest.sha256` **annually**, re-burn a fresh copy every
  **5 years** or immediately on any read/verify failure, and always keep **≥2
  copies** in separate locations. The disc is a redundancy layer, not a hard
  dependency (§10). Documented in `docs/maintenance.md`.

- **Index-loss recovery is a tested first-class path** (issue #21) — the captured
  LTFS index shipped on the recovery disc (`ltfs-index/<barcode>.schema`) is a
  complete byte-level extent map; an archive's bytes can be reconstructed from it
  with raw SCSI `LOCATE`/`READ` and no LTFS mount (`pkg/ltfs` extent extractor +
  `pkg/tape` raw reader), then PAR2-repaired, decrypted, and untarred. The full
  operator procedure is `docs/recovery-procedure.md` (shipped on the disc), backed
  by automated mhvtl recovery tests.

