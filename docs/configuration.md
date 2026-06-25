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
| `copies` | `integer` | yes | Number of identical tape copies to produce. Must be ≥ 1 and ≤ number of drives. Default production value is 2 (one per LTO-6 drive). |
| `library` | `Library` | yes | Tape library hardware and blank tape locations. |
| `redundancy` | `Redundancy` | yes | PAR2 redundancy policy. |
| `encryption` | `Encryption` | yes | age recipient public keys. |
| `delivery` | `Delivery` | yes | Discord webhook for run artifact delivery. |

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

The age private identity corresponding to each recipient must be stored securely. It is
included in the printed report and recovery ISO so the holder can decrypt the tapes
without any online service — see SPEC.md §7.

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
| `DISCORD_FAILURE_WEBHOOK_URL` | no | Discord webhook URL for run failure alerts. When absent, failure alerting is silently disabled. |
