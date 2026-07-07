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
| `label` | `string` | no | Overrides the descriptive on-tape archive directory name (`archives/NNN-<label>/`). When absent, a label is derived from the source's identity (a raw ZFS source's dataset last component, a named k8s resource's name, or its label selector). The value is lowercased and sanitized to `[a-z0-9._-]` (`/`, `@`, `:`, and whitespace become `-`) and truncated to 40 characters, so it need not already be filesystem-safe. It must not be blank when set. It need not be unique — the zero-padded source-index prefix keeps directories distinct. |

\* Exactly one of `k8s` or `zfsPath` must be set.

### K8sRef

Identifies a Kubernetes snapshot resource by GVK (GroupVersionKind), namespace, and
name or label selector. `apiVersion` and `kind` use standard Kubernetes manifest syntax.
Exactly one of `name` or `labelSelector` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | `string` | yes | API group and version, e.g. `snapshot.storage.k8s.io/v1` or `groupsnapshot.storage.k8s.io/v1alpha1`. |
| `kind` | `string` | yes | Resource kind, e.g. `VolumeSnapshot` or `VolumeGroupSnapshot`. A `VolumeGroupSnapshot` is archived as a single tar stream (one subdirectory per member volume). |
| `namespace` | `string` | no† | Kubernetes namespace containing the resource. |
| `name` | `string` | no* | Name of a specific resource. |
| `labelSelector` | `string` | no* | Label selector matching one or more resources (e.g. `app=myapp`). Matches within `namespace` when set; when `namespace` is omitted, it matches across all namespaces (cluster-wide, SPEC §5). |

\* Exactly one of `name` or `labelSelector` must be provided.

† `namespace` is required for a single `name` (a named snapshot has no cluster-wide
meaning). It is optional with a `labelSelector`: omit it to select matching resources
across all namespaces.

Resolution of k8s snapshot references to ZFS dataset paths happens at runtime in the
resolve activity — this config only carries the reference.

Example entries:

```json
{ "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
  "namespace": "plex", "name": "plex-db-snap" }

{ "apiVersion": "groupsnapshot.storage.k8s.io/v1alpha1", "kind": "VolumeGroupSnapshot",
  "namespace": "plex", "labelSelector": "app=plex" }

{ "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
  "labelSelector": "backup=nightly" }
```

The third entry omits `namespace`, so its `labelSelector` matches `VolumeSnapshot`
resources across all namespaces (cluster-wide).

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
| `blankSlots` | `[]integer` | yes | Storage slot numbers that hold usable blank tapes. |
| `tapeCapacityBytes` | `integer` | yes | Native (uncompressed) capacity of one tape, in bytes (e.g. `2500000000000` for LTO-6). Runs plan against native capacity with LTO hardware compression disabled. It is the single-tape ceiling the Resolve feasibility pre-check tests against and the capacity the Pack phase bin-packs into. Must be > 0. |
| `ioWaitTimeoutSeconds` | `integer` | no | How long the Eject phase waits for the operator to clear the import/export station when it fills before failing the run (see below). Omit for the default of 12 hours. Must be > 0 when set. |
| `writeFailureWaitTimeoutSeconds` | `integer` | no | How long the tape path waits for the operator to resume or abort a run paused because a Load or Write failed for one drive-set (see below). Omit for the default of 12 hours. Must be > 0 when set. |
| `allowNonBlankTapes` | `boolean` | no | Opt out of the non-blank-tape refusal so the run may overwrite used tapes (see below). Omit or set `false` (the default) to keep the safety behaviour: a non-blank tape fails the run before any format or write. |

By default a run **never writes to a non-blank tape**: the Load phase confirms every loaded
tape is blank and fails the run before any `mkltfs`/write if one is not, so existing data is
never silently overwritten (SPEC §4.3 step 6). Set `allowNonBlankTapes: true` to deliberately
reclaim used tapes — the run then logs a prominent warning naming each non-blank tape's barcode
and slot and proceeds to format and overwrite it. Blank detection is unchanged; the flag only
changes what happens when a non-blank tape is found, and the overwrite is **irreversible**. Each
overwritten tape is recorded in the run's [PDF report](report.md) so the action is auditable.
The flag is whole-run — it permits overwriting **any** non-blank tape loaded during the run.

When a run writes more physical tapes (logical tapes × copies) than the library has I/O
slots, the Eject phase fills the station and then pauses: it posts an operator alert on the
failure webhook naming the tapes ready for removal, and waits. On libraries that report the
import/export access bit it resumes automatically once the station is cleared and closed;
otherwise the operator runs [`tapectl resume`](tapectl.md) after removing the
tapes. If no one responds within `ioWaitTimeoutSeconds`, the run fails with every written
tape left in an I/O or storage slot (none in a drive).

When a **Load or Write fails for one drive-set**, the tape path does not fail the whole
run. The tapes that wrote successfully are ejected and recorded; the tapes that failed are
ejected too (freeing their drives and emptying their blank slots), and the run pauses,
posting an operator alert on the failure webhook naming the failing phase, the affected
tapes, and the storage slots to restock with fresh blanks. The operator either loads fresh
blank tapes into those slots and runs [`tapectl resume`](tapectl.md) — which
re-drives **only** the failed tapes, never re-formatting a tape already written — or runs
[`tapectl abort`](tapectl.md) to end the run with no further writes. If no one
responds within `writeFailureWaitTimeoutSeconds`, the run fails in that defined paused
state (every tape in a drive, I/O slot, or storage slot) and is reported.

Example (two-drive LTO-6 library, four blank slots, default 12-hour operator timeouts):

```json
{
  "changer": "/dev/sch0",
  "drives": ["/dev/nst0", "/dev/nst1"],
  "blankSlots": [1, 2, 3, 4],
  "tapeCapacityBytes": 2500000000000,
  "ioWaitTimeoutSeconds": 43200,
  "writeFailureWaitTimeoutSeconds": 43200,
  "allowNonBlankTapes": false
}
```

---

## Redundancy

PAR2 redundancy policy. Exactly one of `targetPercentage` or `fillToCapacity` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `targetPercentage` | `number` | no* | Fixed PAR2 redundancy as a percentage of the data size (≥ 0). |
| `fillToCapacity` | `FillConfig` | no* | Expand PAR2 to fill each tape's remaining space down to a minimum floor. |
| `sliceSizeBytes` | `integer` | yes | Fixed size of each encrypted data slice in bytes. The PAR2 block size is derived from this and the tape capacity. Must be > 0. |

\* Exactly one of `targetPercentage` or `fillToCapacity` must be provided.

### FillConfig

Configuration for fill-to-capacity mode.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `floor` | `number` | yes | Minimum PAR2 redundancy percentage (≥ 0). The PAR2 percentage will never be raised below this value even if tape capacity would allow it. |

Example — fixed redundancy (10% PAR2, 4 GiB slices):

```json
{ "targetPercentage": 10, "sliceSizeBytes": 4294967296 }
```

Example — fill-to-capacity (expand PAR2 to fill each tape, never below a 5% floor):

```json
{ "fillToCapacity": { "floor": 5 }, "sliceSizeBytes": 4294967296 }
```

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

Example (one post-quantum recipient and its escrowed identity — placeholder keys, never
real secrets):

```json
{
  "recipients": ["age1pq1exampleonly0publicrecipientkeygoeshere000000000000000000"],
  "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYDONOTUSE00000000000000000000000000"
}
```

---

## Delivery

Delivery of the run's PDF report to Discord on success. The recovery ISO travels on its
burned disc (SPEC §10), so the report is the single delivered artifact.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `webhookUrl` | `string` | yes | Discord incoming webhook URL for success delivery. On success the PDF report is uploaded here. |
| `opticalBurn` | `OpticalBurn` | no | Optionally burn the recovery disc to optical media as an extra redundancy layer (see below). Omit to leave optical burning off. |

### OpticalBurn

Configures burning the recovery disc to optical media (M-DISC DVD; SPEC §10). Burning is
**off by default**: it stays disabled when the section is absent, has no `drives`, or has a
`copies` of zero. It is enabled only when at least one burner drive is listed **and**
`copies` is positive.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `drives` | `[]string` | no | Optical burner device paths (e.g. `/dev/sr0`). Burning is disabled when empty. |
| `copies` | `integer` | no | Number of recovery-disc copies to burn. Zero disables burning; must not be negative. |
| `allowNonBlankDiscs` | `boolean` | no | Opt out of the non-blank-disc refusal so the run may reclaim used discs (see caveat below). Omit or set `false` (the default) to fail the run before burning if a loaded disc is not blank. |
| `burnWaitTimeoutSeconds` | `integer` | no | How long the optical-burn phase waits for the operator to resume or abort a run paused because a burn failed or a non-blank disc was refused. Omit for the default of 12 hours. Must be > 0 when set. |

**Copies are independent of drive count.** As with tape `copies` versus `library.drives`,
the disc `copies` count is intentionally not bounded by the number of `drives`: copies burn
in successive burn-sets of at most `len(drives)` discs at a time. Two burners and three
copies means two discs burn together, then the third.

**`allowNonBlankDiscs` can only reclaim rewritable media.** The flag mirrors
`library.allowNonBlankTapes`, but optical physics limit what it can do: only rewritable media
(DVD±RW / BD-RE) can be erased and re-burned. Write-once media — **DVD-R and M-DISC**, the
archival target — can **never** be overwritten regardless of this flag; a non-blank write-once
disc always fails the burn. A deliberate reclaim is recorded in the run's PDF report.

#### Optical burn operator loop

The Burn phase is operator-in-the-loop, mirroring the tape write-failure pause. Because
there is no optical autoloader, **every burn-set after the first pauses** for you to load
fresh blank discs into the burners and resume. The run also pauses **within a set** on any
burn failure, verify mismatch, or refused non-blank disc. On each pause it posts an alert
on the failure webhook (`DISCORD_FAILURE_WEBHOOK_URL`) naming the run, the burner drive(s),
and the reason, then waits: run [`tapectl resume`](tapectl.md) after loading blank discs to
continue (a within-set pause re-burns only the failed discs; the discs that already verified
are never re-burned), or [`tapectl abort`](tapectl.md) to end the run with no further discs
burned. If no one responds within `burnWaitTimeoutSeconds` (default 12 h), the run fails in
that defined paused state and is reported. After all discs are burned, the delivered
`report.pdf` is re-rendered so it records the burned discs and any overwrite — the copy
inside the burned ISO predates the burn and cannot.

Example (deliver the report to Discord and burn two recovery discs on one burner):

```json
{
  "webhookUrl": "https://discord.com/api/webhooks/123456789/example-token",
  "opticalBurn": {
    "drives": ["/dev/sr0"],
    "copies": 2,
    "allowNonBlankDiscs": false,
    "burnWaitTimeoutSeconds": 43200
  }
}
```

To leave optical burning off, omit `opticalBurn` entirely (or give it no `drives`):

```json
{ "webhookUrl": "https://discord.com/api/webhooks/123456789/example-token" }
```

---

## Complete example configuration

A full, illustrative run config exercising every top-level field — two tape copies, an
LTO-6 library, one Kubernetes snapshot source and one raw ZFS source, fill-to-capacity
PAR2 redundancy, post-quantum encryption, and Discord delivery with optical burning. The
keys below are **placeholders**, not usable secrets.

```json
{
  "copies": 2,
  "feasibilityOverhead": 1.05,
  "sources": [
    {
      "compression": true,
      "k8s": {
        "apiVersion": "snapshot.storage.k8s.io/v1",
        "kind": "VolumeSnapshot",
        "namespace": "plex",
        "name": "plex-db-snap"
      }
    },
    {
      "compression": true,
      "zfsPath": { "name": "bulk-pool-01/media@snap-20240101" }
    }
  ],
  "library": {
    "changer": "/dev/sch0",
    "drives": ["/dev/nst0", "/dev/nst1"],
    "blankSlots": [1, 2, 3, 4],
    "tapeCapacityBytes": 2500000000000,
    "ioWaitTimeoutSeconds": 43200,
    "writeFailureWaitTimeoutSeconds": 43200,
    "allowNonBlankTapes": false
  },
  "redundancy": {
    "fillToCapacity": { "floor": 5 },
    "sliceSizeBytes": 4294967296
  },
  "encryption": {
    "recipients": ["age1pq1exampleonly0publicrecipientkeygoeshere000000000000000000"],
    "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYDONOTUSE00000000000000000000000000"
  },
  "delivery": {
    "webhookUrl": "https://discord.com/api/webhooks/123456789/example-token",
    "opticalBurn": {
      "drives": ["/dev/sr0"],
      "copies": 2
    }
  }
}
```

---

## Operational environment variables

These are set on the worker process, not in the run config, so that infrastructure-level
alerting works even when config parsing fails.

| Variable | Required | Description |
|----------|----------|-------------|
| `ROLE` | yes | Selects which task queue the `worker` binary polls and which activities it registers: `control` (Kubernetes-side: snapshot resolution and bin-packing) or `data` (storage-host-side: tar/age/PAR2/checksum/LTFS/changer activities, plus report/ISO building and Discord delivery — these run on the data worker because the recovery binaries, pinned tools, staged files, and captured LTFS indexes all live there). Matching is case-insensitive. An empty or unrecognized value causes the worker to exit non-zero at startup. |
| `LOG_LEVEL` | no | Logging verbosity for the worker: `debug`, `info`, `warn` (or `warning`), or `error`. Case-insensitive; defaults to `info`, and an unrecognized value also falls back to `info`. |
| `WORKER_IDLE_EXIT_AFTER` | no | **Control worker only.** Go duration (e.g. `15m`); when set to a positive value, the control worker drains in-flight work and exits `0` once it has run no activity for this long, letting a KEDA-spawned Job scale back to zero — see [Control-worker idle-exit](#control-worker-idle-exit). Empty/unset (the default) disables it: the worker runs until `SIGINT`/`SIGTERM`. A negative or unparseable value fails startup. Ignored (inert) for the `data` role. |
| `DISCORD_FAILURE_WEBHOOK_URL` | no | Discord webhook URL for run failure alerts. When absent, failure alerting is silently disabled. |
| `TAPE_K8S_DATASET_PARENT` | no | democratic-csi's `datasetParentName` (e.g. `bulk-pool-01/k8s/democratic-csi/nfs/pvcs`), prepended to a relative CSI `snapshotHandle` to rebuild the absolute ZFS snapshot path during k8s resolution on the control worker. Only needed when a run names k8s sources; when absent, handles are treated as already absolute. |
| `TAPE_STAGING_DIR` | yes (data worker) | Directory the Prepare phase stages prepared archives into — a plain subdirectory of an existing dataset on the storage host (e.g. `/mnt/bulk-pool-01/archive/.tape-staging`), bind-mounted into the data worker container. Each run isolates its output in a subdirectory keyed by run id. Required on the data worker; the Prepare activity fails when it is empty. Ignored by the control worker. |
| `TAPE_RECOVERY_BINARIES_DIR` | yes (data worker) | Directory holding the statically linked recovery binaries (`age`, `par2`, `zstd`, `tar`) staged into the recovery ISO's `/bin` (SPEC §10). Every top-level regular file must be a statically linked native executable; the Report phase fails the run otherwise. Populated in the data worker image to match the pinned recovery-disc tool versions. Ignored by the control worker. |
| `METRICS_ADDR` | no | TCP listen address for the Prometheus `/metrics` endpoint (e.g. `:9090`). The `worker` binary defaults this to `:9090`; set it to an empty value to disable the endpoint entirely — no HTTP server is started and no registry is created. |
| `METRICS_SCRAPE_WAIT_TIMEOUT` | no | Go duration bounding the end-of-run wait for a final Prometheus scrape. Defaults to `60s`; set to `0s` to disable the wait. |
| `HEALTH_ADDR` | no | TCP listen address for the HTTP health endpoints `/healthz` (liveness) and `/readyz` (readiness) — see below. The `worker` binary defaults this to `:8080`; set it to an empty value to disable the endpoints entirely (no port is opened). This is a dedicated always-on port, independent of `METRICS_ADDR`, so health stays available even when `/metrics` is disabled. |
| `TEMPORAL_ADDRESS` | yes | `host:port` of the Temporal frontend gRPC endpoint (e.g. `localhost:7233`). |
| `TEMPORAL_NAMESPACE` | no | Temporal namespace the worker registers under. Defaults to `default` when unset. |
| `TEMPORAL_API_KEY` | no | API key for authenticating to Temporal Cloud. Accepts either an inline token or `file:///absolute/path` — the file form is re-read on every RPC so external rotators can update the file in place without restarting the worker. Non-canonical `file:` forms (missing the third slash, or a relative path) are rejected at startup. |
| `TEMPORAL_TLS` | no | Set to `false` to disable TLS on the Temporal gRPC connection. Useful for local dev stacks; defaults to `true` when `TEMPORAL_API_KEY` is set. |

### Health endpoints

The worker serves two HTTP health endpoints on `HEALTH_ADDR` (default `:8080`) for
Kubernetes probes and the container `HEALTHCHECK`:

| Endpoint | Meaning | Status |
|----------|---------|--------|
| `GET /healthz` | **Liveness** — the process is up and serving. Independent of Temporal connectivity, so a worker merely waiting on Temporal to recover is not restarted. | `200` once the server is listening. |
| `GET /readyz` | **Readiness** — the worker is usefully connected. Re-checks the Temporal frontend health per request. | `200` when Temporal is reachable and healthy; `503` otherwise. |

Neither endpoint gates or fails a run — they are observational only (SPEC §14). The
endpoints are disabled (no port opened) when `HEALTH_ADDR` is set to an empty value.

The `worker healthcheck` subcommand is a self-probe used as the container `HEALTHCHECK`:
it `GET`s `/readyz` on the local health server and exits `0` when ready, non-zero
otherwise, so `docker inspect` health reflects readiness. It never starts a Temporal
worker. It targets `HEALTH_ADDR` by default; an optional positional `host:port` argument
overrides the target.

### Control-worker idle-exit

When `WORKER_IDLE_EXIT_AFTER` is set to a positive duration, the **control worker** exits
once it has been idle for that window, so a KEDA-spawned `Job` can scale back to zero
between runs (parent design: scale-to-zero for the control worker). It is disabled by
default (unset/empty), in which case the worker runs until it receives `SIGINT`/`SIGTERM`,
preserving the fixed-replica `Deployment` posture.

Behavior:

- **Graceful, never abrupt.** The idle timer triggers the *same* drain path as `SIGTERM`:
  the worker stops polling and waits for in-flight tasks to finish before the process
  exits `0`. It never terminates mid-task.
- **Only activity work counts as busy.** The countdown advances only while no activity is
  executing on the worker, and it restarts whenever an activity starts *or* finishes (so a
  long activity does not force an instant exit on return). A workflow that is merely parked
  waiting on a data-worker activity reads as idle — that is intended, so the control worker
  can scale to zero during the hours-long data-side write. Resuming on respawn only replays
  the recorded workflow history (activities are never re-run) and is measured to complete
  in well under a second.
- **Control role only.** For `ROLE=data` the setting is inert (logged and ignored); the
  data worker's lifecycle is unchanged.

Two Prometheus gauges make the behavior observable on `/metrics` when idle-exit is enabled:

| Metric | Meaning |
|--------|---------|
| `tape_archiver_worker_idle_in_flight_tasks` | Activity tasks currently executing on the worker; the idle countdown only advances while this is `0`. |
| `tape_archiver_worker_idle_seconds_until_exit` | Seconds until the worker self-exits on idle: the full idle window while any task is in flight, counting down to `0` once idle. |
