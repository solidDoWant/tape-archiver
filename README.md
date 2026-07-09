# tape-archiver

Back up ZFS snapshots to LTO tape for long-term, offline, cold storage.

A single **run** takes a configuration that fully describes a backup — what to archive,
how many copies, which library slots to use — and produces a set of physically
independent, self-describing tapes, a printed PDF report, and (optionally) an optical
recovery kit. The design target is that a person holding only **the tapes, the printed
report, and the recovery disc** can recover the data in roughly **20 years**, with no
access to this tool, this repository, or any online service.

> **Read [`SPEC.md`](SPEC.md) first.** It is the authoritative technical reference for
> architecture, the run model, package contracts, and configuration. This README is an
> orientation; the spec is the source of truth.

## Design principles

These are non-negotiable and override convenience (see [`SPEC.md`](SPEC.md) §2):

1. **~20-year recoverability** — open standards only (`tar`, `age`, PAR2, LTFS, ISO 9660),
   error correction on the media, and the exact recovery tooling (static binaries +
   source + written procedure) shipped on the recovery disc.
2. **Minimize tape wear** — feed each drive above its speed-matching floor so it streams
   continuously without shoe-shining (back-hitching), and *prove* the rate by benchmark.
3. **Plan for media failure** — multiple copies on multiple tapes, per-archive PAR2
   error correction, and bounded blast radius so one bad segment loses at most one
   recoverable unit.
4. **Everything is tested** — if it is not tested, it does not work.

## How a run works

The run config is the single source of truth. Every run is a **100% full backup** of
everything the config names — no incremental backups, no deduplication, no online catalog,
no cross-run state. Tape inventory is resolved live from the library each run.

**No data is written to tape until the complete contents of every tape are staged and
verified on disk** — this eliminates all computation during the write window, so drives
stream without shoe-shining. The workflow phases:

| Phase | What happens |
|-------|--------------|
| **Resolve** | Expand the config into a concrete work list; resolve k8s `VolumeSnapshot` refs to ZFS snapshots; cheap feasibility pre-check. |
| **Prepare** | Per archive: `tar` → optional `zstd` → `age`-encrypt → split into fixed-size slices → SHA-256. Staged to disk. |
| **Pack** | Bin-pack prepared archives onto tapes by *measured* size, replicated across N copies. |
| **Generate PAR2** | Per-archive PAR2 recovery set (fixed-percentage or fill-to-capacity). |
| **Verify** | Re-read every staged file, verify checksums, confirm each tape's tree fits. A failure blocks all writes. |
| **Load** | `MOVE MEDIUM` blanks into drives; confirm each tape is blank before formatting. |
| **Write** | `mkltfs`, mount LTFS (index sync deferred to unmount), stream the staged tree to tape. Sustained rate monitored. |
| **Eject** | Unmount/unload and export written tapes to the I/O station. |
| **Report** | Build the PDF report (and, if optical burn is enabled, the recovery ISO). |
| **Burn** *(optional)* | Burn and verify the recovery discs. |
| **Deliver** | Post the PDF report to Discord. |

Runs longer than the library's drive count are written as a sequence of **drive-sets**,
bounding concurrent tapes to the number of drives to protect the write-rate floor.
Load/Write failures, a full I/O station, and optical disc swaps **pause for the operator**
(durable single-run workflow state) rather than failing the whole run.

## Architecture

Temporal orchestrates each run across **two task queues**, split by where the data lives:

- **Control worker (`control` queue)** — runs in Kubernetes. Lightweight; resolves the
  config against the k8s API and drives the workflow. Deployed as a `Deployment` or an
  optional KEDA `ScaledJob` that scales to zero between runs.
- **Data worker (`data` queue)** — runs as a container on the storage host, where the
  bulk data already lives. Performs all bulk-data work (`tar`, `age`, PAR2, checksums,
  LTFS, library moves), builds the PDF/ISO, and delivers the report — so bytes never
  cross the network.

The data worker is a **Nix-built OCI image** bundling pinned external tooling (`ltfs`,
`age`, `par2cmdline-turbo`, `zstd`, `mt-st`, `sg3-utils`, `lsscsi`) at the **same
versions shipped on the recovery disc**, so backup and recovery tooling come from one
reproducible source.

See [`SPEC.md`](SPEC.md) §3 for the target environment (LTO-6 library, `bulk-pool-01`
ZFS pool, democratic-csi over NFS).

## Repository layout

```
cmd/          worker (Temporal worker; role selects control/data), tapectl (CLI), gen-config-schema
internal/     config (run-config types + validation), envvar, buildinfo, testutil
pkg/          one concern per package: tape, ltfs, agewrap, par2, archive (tar),
              zfs, k8ssnap, report (PDF), recoverykit (ISO), webhook, checksum,
              logging, metrics, temporalclient
workflows/    backup/ — the backup workflow and activities, split by concern
schemas/      generated JSON config schema (committed)
deploy/       Helm charts (control worker, web UI) + data-worker systemd unit
docs/         operator documentation
nix/          build derivations (ltfs, mhvtl, recovery-binaries, worker + web images)
e2e/          end-to-end tests
```

## Getting started

The dev environment is provisioned via Nix flakes:

```sh
nix develop        # enter the dev shell with all tooling
make build         # build binaries into bin/
make test          # unit tests, -race
```

Common Make targets (see the [`Makefile`](Makefile) for the full list):

| Target | Purpose |
|--------|---------|
| `make build` | Build binaries into `bin/`. |
| `make fmt` / `make vet` / `make lint` | Format, vet, lint (`lint-fix` to autofix). |
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Integration tests against `mhvtl` + dev Temporal. |
| `make test-e2e` | End-to-end tests. |
| `make benchmark` | Write-rate / shoe-shining benchmarks (real hardware) — **not yet implemented** (stub; exits non-zero). |
| `make generate-schema` | Regenerate the committed config JSON schema. |
| `make build-images` | Build the data-worker, control-worker, and web OCI images via Nix. |
| `make helm` | Package the control-worker and web Helm charts. |
| `make temporal-up` / `make mhvtl-up` / `make zpool-up` | Bring up local Temporal, a virtual tape library, and an ephemeral ZFS pool for tests. |

### Dry-run and the virtual library

Dry-run is a first-class mode. Device targets are configurable, so a dry-run points the
worker at a virtual tape library (**`mhvtl`**) instead of real `/dev/sch0` + `/dev/nstX`
devices, exercising the same load → format → write → eject code path end to end. This is
shared with the integration test suite.

## Testing

Layered and build-tag-gated (see [`SPEC.md`](SPEC.md) §13):

- **Unit** (default, `-race`) — pure logic: planning/bin-packing, config parsing, slice
  sizing, PAR2 block-size computation, SCSI element-status decoding.
- **Integration** (`//go:build integration`) — the full tape path against `mhvtl` and a
  dev Temporal; `pkg/zfs` against an ephemeral file-backed ZFS pool. Skips when required
  devices/env are absent.
- **Real-hardware + benchmark** (build-tag gated, env-skipped) — measures sustained write
  MB/s and scrapes drive log pages for back-hitch / TapeAlert flags to prove the
  anti-shoe-shining rate before the tool is trusted in production.
- **End-to-end** (`//go:build e2e`) — the whole workflow including report/ISO/delivery
  against virtual hardware.

Acceptance criteria are Given/When/Then describing observable behavior only; tests are
never modified to pass — the code is fixed instead.

## On-tape format

All formats are open and widely implemented. Each tape is an **LTFS** filesystem with a
readable index:

```
archives/NNN-<label>/   one directory per archive: age-encrypted, optionally
                        zstd-compressed tar slices + a PAR2 recovery set
manifest.json           top-level checksum manifest, written LAST — its presence
                        signals the tape was completely written
```

Tapes are identified by their **library-read barcode** (SCSI volume tag), which `mkltfs`
also sets as the LTFS volume name. Two independent redundancy mechanisms protect the
data: **PAR2** within a tape (localized decay) and **N copies** across tapes (whole-tape
loss). Encryption uses `age` with native **post-quantum** recipients (hybrid ML-KEM-768);
the private identity is escrowed in the printed report and on the recovery disc.

## Documentation

Operator and reference docs live under [`docs/`](docs/):

- [`configuration.md`](docs/configuration.md) — run-config reference.
- [`tapectl.md`](docs/tapectl.md) — the CLI (submit / inspect / resume / abort runs).
- [`recovery-procedure.md`](docs/recovery-procedure.md) — step-by-step recovery (shipped on the disc).
- [`recovery-iso.md`](docs/recovery-iso.md) — the optical recovery kit.
- [`maintenance.md`](docs/maintenance.md) — barcode convention, re-burn cadence, operator procedures.
- [`report.md`](docs/report.md) — the PDF report contents.
- [`control-worker-helm.md`](docs/control-worker-helm.md), [`control-worker-image.md`](docs/control-worker-image.md), [`data-worker-image.md`](docs/data-worker-image.md) — deployment.
- [`web-ui.md`](docs/web-ui.md) — the web UI (submit / monitor / pause actions / run history).
- [`web-helm.md`](docs/web-helm.md), [`web-image.md`](docs/web-image.md) — web UI deployment.

## License

See [`LICENSE`](LICENSE).
