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
  attached here. Has `mt`, `mtx`, `zfs`, `lsscsi`, `sg_map`, `sg3-utils`. Does **not**
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

- **Control worker (`control` queue) — runs in Kubernetes.** Lightweight, no bulk data.
  Resolves the run config against the k8s API (snapshot discovery/validation), drives
  the workflow, builds the PDF report and the recovery ISO, and delivers to Discord.
- **Data worker (`data` queue) — runs as a container on `ubuntu-storage-host-01`.**
  Performs all bulk-data activities where the bytes already are, so they never cross
  the network: `tar`, `age`, PAR2 slicing, checksums, LTFS format/mount/write, and
  library moves.

The data worker is a **Nix-built OCI image** (`streamLayeredImage`, per media-processor)
run by systemd-managed Docker on the host. The image bundles pinned tooling — `ltfs`,
`age` (>= 1.3.1), `par2` (par2cmdline-turbo), `zstd`, `mt-st`, `mtx`, `sg3-utils`,
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
  and the recovery ISO. Restore depends on zero online services.
- **Tape inventory is resolved live, per run.** At startup the tool reads the library
  with `mtx status`; the config declares which storage elements hold usable blank
  tapes. Written tapes are exported to the I/O station at the end of the run; the
  operator reloads blanks before the next run.

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
   cannot fit on one tape *before* doing real work. This is an estimate, not the plan.
2. **Prepare.** For each archive: `tar` the snapshot contents → optional `zstd`
   compression → `age`-encrypt → split into fixed-size slices → compute SHA-256
   checksums. All output is staged to disk and its exact size measured.
3. **Pack.** Bin-pack the prepared archives onto tapes by their *measured* size
   (≤ tape capacity, accounting for PAR2 and LTFS overhead), replicated across N copies
   (N = configured copy count, ≤ number of drives for parallel writing). Plan against
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
6. **Load.** Move the selected blank tapes from their storage slots into the drives
   (`mtx`), and confirm each loaded tape is blank/empty before formatting — a run must
   never silently overwrite existing data.
7. **Write.** `mkltfs` each tape (setting the LTFS volume name to the tape's barcode),
   mount LTFS **with index sync deferred to unmount** (`-o sync_type=unmount`), and
   stream the staged tree to tape. The N copies write to N drives in parallel. Writing
   is a pure sequential disk→tape copy whose sustained rate is monitored. The LTFS index
   is therefore written **once**, at unmount during Eject, rather than periodically
   during the write — see §14. A per-tape checksum/manifest file is written last; the
   LTFS index is read back after unmount and captured for the ISO.
8. **Eject.** Unmount/unload each written tape and transfer it to an I/O station slot
   for physical removal.
9. **Report.** Build the PDF report (§9) and the recovery ISO (§10).
10. **Deliver.** Send the report and ISO to Discord via webhook (§11).

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
  drive count; copies write in parallel, one per drive).
- **Library** — device targets (real `/dev/sch0` + `/dev/nstX`, or a virtual library
  for dry-run, §12) and the list of storage slots holding usable blank tapes.
- **Redundancy** — PAR2 policy: a target redundancy percentage, or **fill-to-capacity**
  (size data first, then expand PAR2 to consume remaining tape down to a configured
  floor). Slice size is configurable.
- **Compression** — optional `zstd` before encryption, configurable per source,
  default on. Already-compressed sources (e.g. `media`) gain little but are unharmed.
- **Encryption** — the age recipient(s) (`age1pq1…`) to encrypt to.
- **Delivery** — the Discord webhook target and report/ISO options.

## 6. On-tape layout and formats

All formats are open and widely implemented, for 20-year recoverability.

- **Container:** **LTFS** (LinearTape-Open `ltfs`) presents each tape as a
  self-describing filesystem with a readable index. Files are stored as regular files;
  a copy of the LTFS index is also captured to the recovery ISO in case the on-tape
  index is damaged.
- **Tape identity:** the library-read **barcode (volume tag from `mtx`) is the
  canonical physical ID**. `mkltfs` sets the LTFS volume name to the barcode; the
  per-tape manifest and the report reference tapes by barcode. (Production tapes are
  barcode-labeled and read by the library.)
- **Directory layout within LTFS:**
  - `archives/NNN/` — one directory per archive, where `NNN` is the zero-padded source
    index (e.g. `archives/000/`, `archives/001/`). Each contains: the fixed-size
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

## 7. Encryption and key management

- **Tool:** mainline `age` >= **1.3.1**, using its **native post-quantum recipients**
  (HPKE with hybrid **ML-KEM-768**; never weaker than X25519 alone). Keys are generated
  with `age-keygen -pq`; recipients are `age1pq1…`, identities `AGE-SECRET-KEY-PQ-1…`.
  No plugin is required. The age binary, its source, and the C2SP format spec are
  bundled on the recovery ISO.
- **Key escrow (operator decision):** the **private identity is included** in the
  printed report and on the recovery ISO, so the holder of those artifacts can always
  decrypt. Consequence to document plainly: the report and ISO delivered to Discord
  therefore contain the decryption secret and must be handled accordingly. (Treated as
  acceptable for this personal cold-storage threat model.)

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

Produced by the control worker. Contains, at minimum: run id and date; full contents
manifest (archives, member volumes, source snapshots, sizes, SHA-256 checksums);
which physical tape(s) (by barcode/label) hold what; how the tapes were built (tool
version, `age`/`par2`/`ltfs` versions, slice size, PAR2 redundancy, drive/library
identifiers); the **age private identity**; and the recovery procedure. Intended to be
printed and laminated as the durable offline index for the run.

## 10. Recovery ISO (optical)

An ISO 9660 image (compressed) that is the self-contained recovery kit. Contains: the
PDF report; the full SHA-256 manifest; a backup copy of each tape's LTFS index; and the
**recovery tooling** — static `age`/`par2`/`zstd`/`tar` (and LTFS read instructions)
plus their source and a written, step-by-step recovery procedure — so the tapes can be
read, repaired, decrypted, decompressed, and unpacked with only the disc and the tapes.

**Target media: M-DISC DVD.** Its inorganic recording layer is ISO/IEC 10995-tested and
NIST-listed for 100+ year archival life, and — unlike recordable Blu-ray, whose media
and drive production are being discontinued (Sony exited recordable BD in Feb 2025 with
"no successor") — it is readable in the large, long-lived installed base of DVD drives.
The ISO is tens of MB, so DVD capacity is ample. Burn **at least two copies** and verify
each by reading back and comparing against the manifest. Optical is one redundancy layer,
not a hard dependency: the laminated report independently carries the key, procedure, and
manifest, and every tape carries its own LTFS index and checksums. Burning is a manual
operator step; periodic re-burn/refresh is a documented maintenance task.

## 11. Notifications (Discord)

There are two distinct Discord notification paths:

**Success delivery (per-run, configured in the run config).** At the end of a
successful run the control worker delivers the report and the (compressed) ISO to the
Discord webhook named in the run config (§5 Delivery). Assumption: the compressed ISO
fits the webhook upload limit (~25 MB); if a future run exceeds it, revisit (e.g. post
the report plus a checksum and fetch the ISO from the pool). Out of scope for now.

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
  sizing, PAR2 block-size computation, parsing of `mtx`/`mt`/`sg_logs` output.
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
  staged before writing) and **verified by benchmark** against real hardware.
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
- `pkg/` — one concern per package: `tape` (mtx/mt/changer), `ltfs`, `agewrap`, `par2`,
  `archive` (tar), `zfs`, `k8ssnap` (VolumeSnapshot discovery), `report` (PDF),
  `recoverykit` (ISO), `webhook` (Discord), `checksum`, `logging`, `metrics`,
  `temporalclient`.
- `internal/config` — run-config types and env parsing.
- `workflows/backup/` — the backup workflow and activities, split by concern
  (`resolve.go`, `plan.go`, `prepare.go`, `verify.go`, `write.go`, `library.go`,
  `report.go`, `deliver.go`) with co-located tests.
- `schemas/` — generated config schema. `deploy/charts/` — Helm chart for the control
  worker. `docs/` — operator docs. `e2e/` — end-to-end tests.

## 16. Resolved decisions and remaining open items

The following were settled during bootstrap (see §3, §4.3, §6, §8, §10):

- **Tape identity** — library barcode (mtx volume tag) is the canonical ID; LTFS volume
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
  a generous margin for many-small-file datasets) and is tunable per run via the
  `feasibilityOverhead` config field. The PAR2 fraction is the target percentage, or the
  floor in fill-to-capacity mode. This is only the pre-check estimate; the authoritative
  size is the measured staged size from Prepare.
- **LTFS implementation** — the reference open-source **LinearTape-Open `ltfs`**
  (IBM-maintained, multi-vendor, Apache-2.0), pinned at **v2.4.8.4** and built from a Nix
  derivation (`nix/ltfs.nix`); the worker image and recovery disc ship the same version.
  The vendor-locked `hpe-ltfs` was rejected: it refuses non-HPE drives (so it cannot be
  tested against the mhvtl IBM-emulated drive), and the production LTO-6 drives are IBM.
  The reference `ltfs` drives the tape through its **`sg` backend (the `/dev/sg*` node)**,
  not the `nst` node — both are already passed through to the data worker (§4.1).

Remaining open / future work:

- Tape barcode *format* convention (any project prefix/sequence is cosmetic; the
  canonical ID is whatever the library reads).
- Recovery-disc re-burn/refresh cadence as a documented maintenance task.

