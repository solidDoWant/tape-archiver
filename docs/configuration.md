# Run Configuration

A tape-archiver run is fully described by a configuration supplied as a JSON file or as
the Temporal workflow payload. Every field is documented here; the committed JSON Schema
at `schemas/run-config.schema.json` is the machine-readable source of truth.

Regenerate the schema after any config-type change:

```
make generate-schema
```

Verify the committed schema is up-to-date (CI check):

```
make check-schema
```

---

## Top-level fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sources` | `[]Source` | yes | Items to archive. At least one required. |
| `copies` | `integer` | yes | Number of identical tape copies to produce. Must be ≥ 1. May exceed the number of drives — copies beyond the drive count are written across successive drive-sets. The library must hold one blank tape per physical tape written (logical tapes × copies). Default production value is 2 (one per LTO-6 drive). |
| `library` | `Library` | yes | Tape library hardware and blank tape locations. |
| `redundancy` | `Redundancy` | yes | PAR2 redundancy policy. |
| `encryption` | `Encryption` | yes | age recipient public keys. |
| `delivery` | `Delivery` | yes | Discord webhook for run artifact delivery. |
| `feasibilityOverhead` | `number` | no | Multiplier (≥ 1) inflating each source's estimated size in the Resolve feasibility pre-check. Defaults to `1.05` when absent. |

### feasibilityOverhead

The Resolve phase runs a cheap pre-check that rejects any single archive too large
to fit on one tape *before* any data is staged (SPEC.md §4.3 phase 1). It estimates an
archive's on-tape size as:

```
estimate = zfs logicalreferenced × feasibilityOverhead × (1 + PAR2 fraction)
```

`feasibilityOverhead` covers the framing the pipeline adds on top of the raw data —
`tar` headers/padding and `age` STREAM chunk overhead. `zstd` compression is assumed to
give *no* size reduction (the incompressible worst case), so the estimate never runs
low. The default of **1.05** (5%) is a deliberately generous margin; raise it for
datasets of very many small files, where `tar` per-file overhead is a larger fraction of
the total. This tunes only the pre-check — the authoritative size is the measured staged
size produced by the Prepare phase, never this estimate.

---

## Source

Each element of `sources` archives exactly one item — a Kubernetes snapshot resource or
a raw ZFS dataset/snapshot. Exactly one of `k8s` or `zfsPath` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `compression` | `boolean` | no | Enable zstd compression before encryption. Defaults to `true` when absent. |
| `k8s` | `K8sRef` | no* | Reference to a Kubernetes snapshot resource. |
| `zfsPath` | `ZFSPathSource` | no* | Explicit ZFS dataset or snapshot name. |

\* Exactly one of `k8s` or `zfsPath` must be set.

### K8sRef

Identifies a Kubernetes snapshot resource by GVK (GroupVersionKind), namespace, and
name or label selector. `apiVersion` and `kind` use standard Kubernetes manifest syntax.
Exactly one of `name` or `labelSelector` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | `string` | yes | API group and version, e.g. `snapshot.storage.k8s.io/v1` or `groupsnapshot.storage.k8s.io/v1alpha1`. |
| `kind` | `string` | yes | Resource kind, e.g. `VolumeSnapshot` or `VolumeGroupSnapshot`. A `VolumeGroupSnapshot` is archived as a single tar stream (one subdirectory per member volume). |
| `namespace` | `string` | yes | Kubernetes namespace containing the resource. |
| `name` | `string` | no* | Name of a specific resource. |
| `labelSelector` | `string` | no* | Label selector matching one or more resources within `namespace` (e.g. `app=myapp`). |

\* Exactly one of `name` or `labelSelector` must be provided.

Resolution of k8s snapshot references to ZFS dataset paths happens at runtime in the
resolve activity — this config only carries the reference.

Example entries:

```json
{ "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
  "namespace": "plex", "name": "plex-db-snap" }

{ "apiVersion": "groupsnapshot.storage.k8s.io/v1alpha1", "kind": "VolumeGroupSnapshot",
  "namespace": "plex", "labelSelector": "app=plex" }
```

### ZFSPathSource

An explicit ZFS dataset or snapshot by name.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | yes | ZFS dataset or snapshot name, e.g. `bulk-pool-01/archive@snap-20240101` or `bulk-pool-01/media`. |

---

## Library

Specifies the SCSI changer, drives, and which storage slots hold blank tapes.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `changer` | `string` | yes | SCSI changer device path (e.g. `/dev/sch0`) or a virtual library path for dry-run. |
| `drives` | `[]string` | yes | Tape drive device paths. Prefer the non-rewinding nodes (`/dev/nst0`, `/dev/nst1`). |
| `blankSlots` | `[]integer` | yes | Storage slot numbers (from `mtx status`) that hold usable blank tapes. |
| `tapeCapacityBytes` | `integer` | yes | Native (uncompressed) capacity of one tape, in bytes (e.g. `2500000000000` for LTO-6). Runs plan against native capacity with LTO hardware compression disabled. It is the single-tape ceiling the Resolve feasibility pre-check tests against and the capacity the Pack phase bin-packs into. Must be > 0. |
| `ioWaitTimeoutSeconds` | `integer` | no | How long the Eject phase waits for the operator to clear the import/export station when it fills before failing the run (see below). Omit for the default of 12 hours. Must be > 0 when set. |

When a run writes more physical tapes (logical tapes × copies) than the library has I/O
slots, the Eject phase fills the station and then pauses: it posts an operator alert on the
failure webhook naming the tapes ready for removal, and waits. On libraries that report the
import/export access bit it resumes automatically once the station is cleared and closed;
otherwise the operator runs [`tapectl resume <run-id>`](tapectl.md) after removing the
tapes. If no one responds within `ioWaitTimeoutSeconds`, the run fails with every written
tape left in an I/O or storage slot (none in a drive).

---

## Redundancy

PAR2 redundancy policy. Exactly one of `targetPercentage` or `fillToCapacity` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `targetPercentage` | `number` | no* | Fixed PAR2 redundancy as a percentage of the data size (≥ 0). |
| `fillToCapacity` | `FillConfig` | no* | Expand PAR2 to fill each tape's remaining space down to a minimum floor. |
| `sliceSizeBytes` | `integer` | yes | Fixed size of each encrypted data slice in bytes. The PAR2 block size is derived from this and the tape capacity. |

\* Exactly one of `targetPercentage` or `fillToCapacity` must be provided.

### FillConfig

Configuration for fill-to-capacity mode.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `floor` | `number` | yes | Minimum PAR2 redundancy percentage (≥ 0). The PAR2 percentage will never be raised below this value even if tape capacity would allow it. |

---

## Encryption

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `recipients` | `[]string` | yes | One or more age public keys (`age1pq1…` for post-quantum recipients, generated with `age-keygen -pq`). Archives are encrypted to all recipients. |
| `identity` | `string` | yes | The age private identity (`AGE-SECRET-KEY-PQ-1…`) escrowed into the report and recovery ISO. **Never used to encrypt** — encryption uses `recipients` only. The Report phase fails the run if it is empty or if its derived public key is not among `recipients`. |

The `identity` is included in the printed report and recovery ISO so the holder can
decrypt the tapes without any online service (SPEC.md §7 key-escrow decision). Because
those artifacts therefore carry the decryption secret and are delivered to Discord on
success, store and dispose of them accordingly. `identity` must be one of the private
identities matching a configured `recipient`; the run refuses to build a report that
escrows a key that cannot decrypt the archives.

---

## Delivery

Delivery of run artifacts (PDF report and recovery ISO) to Discord on success.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `webhookUrl` | `string` | yes | Discord incoming webhook URL for success delivery. |

---

## Operational environment variables

These are set on the worker process, not in the run config, so that infrastructure-level
alerting works even when config parsing fails.

| Variable | Required | Description |
|----------|----------|-------------|
| `ROLE` | yes | Selects which task queue the `worker` binary polls and which activities it registers: `control` (Kubernetes-side: snapshot resolution and bin-packing) or `data` (storage-host-side: tar/age/PAR2/checksum/LTFS/changer activities, plus report/ISO building and Discord delivery — these run on the data worker because the recovery binaries, pinned tools, staged files, and captured LTFS indexes all live there). Matching is case-insensitive. An empty or unrecognized value causes the worker to exit non-zero at startup. |
| `LOG_LEVEL` | no | Logging verbosity for the worker: `debug`, `info`, `warn` (or `warning`), or `error`. Case-insensitive; defaults to `info`, and an unrecognized value also falls back to `info`. |
| `DISCORD_FAILURE_WEBHOOK_URL` | no | Discord webhook URL for run failure alerts. When absent, failure alerting is silently disabled. |
| `TAPE_K8S_DATASET_PARENT` | no | democratic-csi's `datasetParentName` (e.g. `bulk-pool-01/k8s/democratic-csi/nfs/pvcs`), prepended to a relative CSI `snapshotHandle` to rebuild the absolute ZFS snapshot path during k8s resolution on the control worker. Only needed when a run names k8s sources; when absent, handles are treated as already absolute. |
| `TAPE_STAGING_DIR` | yes (data worker) | Directory the Prepare phase stages prepared archives into — a plain subdirectory of an existing dataset on the storage host (e.g. `/mnt/bulk-pool-01/archive/.tape-staging`), bind-mounted into the data worker container. Each run isolates its output in a subdirectory keyed by run id. Required on the data worker; the Prepare activity fails when it is empty. Ignored by the control worker. |
| `TAPE_RECOVERY_BINARIES_DIR` | yes (data worker) | Directory holding the statically linked recovery binaries (`age`, `par2`, `zstd`, `tar`) staged into the recovery ISO's `/bin` (SPEC §10). Every top-level regular file must be a statically linked native executable; the Report phase fails the run otherwise. Populated in the data worker image to match the pinned recovery-disc tool versions. Ignored by the control worker. |
| `METRICS_ADDR` | no | TCP listen address for the Prometheus `/metrics` endpoint (e.g. `:9090`). The `worker` binary defaults this to `:9090`; set it to an empty value to disable the endpoint entirely — no HTTP server is started and no registry is created. |
| `METRICS_SCRAPE_WAIT_TIMEOUT` | no | Go duration bounding the end-of-run wait for a final Prometheus scrape. Defaults to `60s`; set to `0s` to disable the wait. |
| `TEMPORAL_ADDRESS` | yes | `host:port` of the Temporal frontend gRPC endpoint (e.g. `localhost:7233`). |
| `TEMPORAL_NAMESPACE` | no | Temporal namespace the worker registers under. Defaults to `default` when unset. |
| `TEMPORAL_API_KEY` | no | API key for authenticating to Temporal Cloud. Accepts either an inline token or `file:///absolute/path` — the file form is re-read on every RPC so external rotators can update the file in place without restarting the worker. Non-canonical `file:` forms (missing the third slash, or a relative path) are rejected at startup. |
| `TEMPORAL_TLS` | no | Set to `false` to disable TLS on the Temporal gRPC connection. Useful for local dev stacks; defaults to `true` when `TEMPORAL_API_KEY` is set. |
