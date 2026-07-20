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

A bare dataset name (no `@`) is archived from the dataset's live mountpoint. The dataset
must be mounted: if it is not, the run fails during Prepare — before any tape is written —
rather than archiving whatever directory shadows the (unmounted) mountpoint, which would
silently certify a stale or empty archive.

---

## Library

Specifies the SCSI changer, drives, and which storage slots hold blank tapes.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `changer` | `string` | yes | SCSI changer device path (e.g. `/dev/sch0`) or a virtual library path for dry-run. |
| `drives` | `[]string` | yes | Tape drive device paths. Prefer the non-rewinding nodes (`/dev/nst0`, `/dev/nst1`). Each path must be non-blank and distinct — blank or duplicate entries fail validation. Order is not significant and need not match the changer's data-transfer element order: the Load phase pairs each device node to its changer element by the drive's unit serial (read from the drive's INQUIRY and the changer's `READ ELEMENT STATUS` DVCID identifier), so a kernel probe order that assigns `/dev/nst0` to the second drive still loads, blank-checks, and writes each tape on the drive it was assigned to. |
| `blankSlots` | `[]integer` | yes | Storage slot numbers that hold usable blank tapes. Each entry must be non-negative and distinct — a negative or duplicate slot address fails validation. The number of slots must also be a positive integer multiple of `copies`: every logical tape needs one blank per copy (physical tapes = logical tapes × copies), so a count that does not divide evenly by `copies` leaves blanks that can never complete another logical tape's copy set, and fails validation. For example, with `copies: 3` supply 3, 6, 9, … blank slots. |
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
| `targetPercentage` | `number` | no* | Fixed PAR2 redundancy as a percentage of the data size. Must be a whole number in the inclusive range 1–100 (the range the PAR2 engine supports); out-of-range or fractional values are rejected up front. |
| `fillToCapacity` | `FillConfig` | no* | Expand PAR2 to fill each tape's remaining space down to a minimum floor. |
| `sliceSizeBytes` | `integer` | yes | Fixed size of each encrypted data slice in bytes. The PAR2 block size is derived from each archive's data size and slice count (targeting ~2,000 source blocks, one per slice at high slice counts). Must be > 0. Additionally bounded relative to the resolved source size: a value so small that the run's total slice count would grow an activity payload past Temporal's ~2 MB limit is rejected up front during the Resolve phase, before any staging, with an error naming `sliceSizeBytes` and a suggested minimum. See [Slice size and payload bound](#slice-size-and-payload-bound). |

\* Exactly one of `targetPercentage` or `fillToCapacity` must be provided.

### FillConfig

Configuration for fill-to-capacity mode.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `floor` | `number` | yes | Minimum PAR2 redundancy percentage. Must be a whole number in the inclusive range 1–100 (the range the PAR2 engine supports); out-of-range or fractional values are rejected up front. The PAR2 percentage will never be raised below this value even if tape capacity would allow it. |

Example — fixed redundancy (10% PAR2, 4 GiB slices):

```json
{ "targetPercentage": 10, "sliceSizeBytes": 4294967296 }
```

Example — fill-to-capacity (expand PAR2 to fill each tape, never below a 5% floor):

```json
{ "fillToCapacity": { "floor": 5 }, "sliceSizeBytes": 4294967296 }
```

### Slice size and payload bound

Every staged slice carries a small metadata record (path, SHA-256, size) that rides
inside Temporal activity payloads throughout the run. Temporal caps a single payload
at roughly 2 MB, so the run's total slice count is bounded: `sliceSizeBytes` too small
for the resolved source size would produce so many slices that a payload would exceed
that limit only after up to a day of staging.

To surface this as a configuration error instead of a late, generic
payload-too-large failure, the Resolve phase estimates the whole-run slice count from
the resolved source size and rejects a too-small `sliceSizeBytes` **before any
staging begins**. The error names `sliceSizeBytes`, states the source-size
relationship, and suggests a minimum value. A comfortably-sized slice (for example,
1 GiB slices on a few-terabyte source) is never affected. Raising `sliceSizeBytes`
per the suggestion resolves it.

The other large post-write artifact — each physical tape's captured LTFS index, which
grows with the on-tape file count and can reach several megabytes — does not count
against this bound: it is staged to disk and passed to the Report phase by path rather
than carried in an activity payload, so it never inflates a payload regardless of run
size.

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

**`allowNonBlankDiscs` never reclaims a disc this run itself just burned.** The flag exists to
reclaim a genuinely old disc left in a drive from a *prior* run. A non-blank rewritable disc in
a burner that has already produced a verified copy **this** run — for example one still loaded
because a between-set disc swap was resumed without swapping that drive's disc — is that copy,
not a leftover. The run refuses to blank it (even with `allowNonBlankDiscs: true`) and pauses
for you to load a fresh blank, so every copy the delivered report counts exists on its own
distinct physical disc.

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
| `TAPE_RECOVERY_BINARIES_DIR` | image default | Directory holding the statically linked recovery binaries (`age`, `par2`, `zstd`, `tar`) staged into the recovery ISO's `/bin` (SPEC §10). Every top-level regular file must be a statically linked native executable; the Report phase fails the run otherwise. The data worker image bakes the recovery set in at `/recovery/bin` and defaults this variable there — from the same pinned nixpkgs as the rest of the tooling, so the disc cannot drift from the write path. Override only to relocate the set. Ignored by the control worker. |
| `TAPE_RECOVERY_SOURCES_DIR` | image default | Directory holding the recovery tools' upstream source archives staged into the recovery ISO's `/src` (SPEC §2, §10 — "…plus their source"). It is the sibling `$out/src` of `TAPE_RECOVERY_BINARIES_DIR`'s `$out/bin` from `nix/recovery-binaries.nix`. Its top-level regular files are staged verbatim (not linkage-checked — these are archives, not executables); the Report phase fails the run if it is empty or yields no files, so a disc can never silently ship without the source needed to rebuild the tools on future hardware. The data worker image bakes it in at `/recovery/src` and defaults this variable there, alongside the binaries. Override only to relocate the set. Ignored by the control worker. |
| `METRICS_ADDR` | no | TCP listen address for the Prometheus `/metrics` endpoint (e.g. `:9090`). The `worker` binary defaults this to `:9090`; set it to an empty value to disable the endpoint entirely — no HTTP server is started and no registry is created. |
| `METRICS_SCRAPE_WAIT_TIMEOUT` | no | Go duration bounding the end-of-run wait for a final Prometheus scrape. Defaults to `60s`; set to `0s` to disable the wait. |
| `HEALTH_ADDR` | no | TCP listen address for the HTTP health endpoints `/healthz` (liveness) and `/readyz` (readiness) — see below. The `worker` binary defaults this to `:8080`; set it to an empty value to disable the endpoints entirely (no port is opened). This is a dedicated always-on port, independent of `METRICS_ADDR`, so health stays available even when `/metrics` is disabled. |
| `TEMPORAL_ADDRESS` | yes | `host:port` of the Temporal frontend gRPC endpoint (e.g. `localhost:7233`). |
| `TEMPORAL_NAMESPACE` | no | Temporal namespace the worker registers under. Defaults to `default` when unset. |
| `TEMPORAL_API_KEY` | no | API key for authenticating to Temporal Cloud. Accepts either an inline token or `file:///absolute/path` — the file form is re-read on every RPC so external rotators can update the file in place without restarting the worker. Non-canonical `file:` forms (missing the third slash, or a relative path) are rejected at startup. |
| `TEMPORAL_TLS` | no | Set to `false` to disable TLS on the Temporal gRPC connection. Useful for local dev stacks; defaults to `true` when `TEMPORAL_API_KEY` is set. |

### Web UI environment variables (`cmd/web`)

The `web` binary (the browser UI's HTTP server — see `docs/web-ui-design.md`) is a
separate process from `worker`/`tapectl` and reads its own environment variables, though
it shares the Temporal client factory (`pkg/temporalclient`) and the
`METRICS_ADDR`/`HEALTH_ADDR` conventions with `worker`. It serves the SPA, a read-only
JSON API under `/api/*` (listing/describing backup runs via Temporal visibility), and
run submission (including dry-run), live monitoring (Server-Sent Events), and operator
resume/abort actions, gated behind OIDC authentication (`pkg/webauth`) — see
[OIDC authentication](#oidc-authentication-cmdweb) below.

| Variable | Required | Description |
|----------|----------|--------------|
| `WEB_LISTEN_ADDRESS` | no | TCP listen address for the web UI's main HTTP server — the SPA at `/` and the JSON API under `/api/*` (e.g. `:8080` or `127.0.0.1:8080`). Defaults to `:8080` when unset or empty. |
| `HEALTH_ADDR` | no | TCP listen address for the HTTP health endpoints `/healthz` (liveness) and `/readyz` (readiness — reflects Temporal connectivity) — see [Health endpoints](#health-endpoints) below. **Defaults to `:8081`** for `cmd/web` — deliberately different from `worker`'s `:8080` default, since (unlike the worker) `cmd/web`'s main port already answers real traffic on its own `:8080` default; set to an empty value to disable the endpoints entirely. |
| `METRICS_ADDR` | no | TCP listen address for the Prometheus `/metrics` endpoint, including Temporal SDK client metrics. Defaults to `:9090`, the same default `worker` uses — safe to share since `cmd/web` runs as its own Kubernetes Deployment/pod, not colocated with the worker. Set to an empty value to disable the endpoint entirely. |
| `METRICS_SCRAPE_WAIT_TIMEOUT` | no | Same mechanics as the worker's setting above (bounds the shutdown-time wait for a final Prometheus scrape before `cmd/web` shuts its `/metrics` server down), but a different default: **`0s` — the wait is skipped entirely**. Unlike the worker, whose end-of-run metrics only exist at exit, a long-running web server loses at most one scrape interval of counter increments at shutdown, and waiting would hold every SIGTERM drain (e.g. each pod in a rolling deploy) open for up to the full timeout. Set to a positive duration (e.g. `60s`) to opt back into the wait. With the default, `cmd/web` exits within roughly its 10-second HTTP drain deadline of receiving SIGINT/SIGTERM — normally far sooner — plus whatever value you set here. |
| `TEMPORAL_ADDRESS` / `TEMPORAL_NAMESPACE` / `TEMPORAL_API_KEY` / `TEMPORAL_TLS` | yes (`TEMPORAL_ADDRESS`) | Same envconfig-driven Temporal client settings documented above for `worker`/`tapectl` (`pkg/temporalclient`) — `cmd/web` connects to the same Temporal frontend to serve `/api/runs` and `/api/runs/{runID}`. |
| `TEMPORAL_UI_URL` | no | Base URL of the browsable [Temporal Web UI](https://docs.temporal.io/web-ui) (e.g. `https://temporal.example.com` or `http://localhost:8233`). When set, each run's overview page shows a "Temporal workflow ↗" link straight to that run's workflow-history view (`{TEMPORAL_UI_URL}/namespaces/{namespace}/workflows/backup/{runId}/history`), where `{namespace}` is resolved from the same Temporal client config profile `cmd/web` dials with (`TEMPORAL_NAMESPACE` / `TEMPORAL_CONFIG_FILE`). Unset (the default) means no such link is shown — nothing else changes. Surfaced to the SPA by the session-gated `GET /api/config/ui` route (see below). |
| `LOG_LEVEL` | no | Same semantics as the worker's `LOG_LEVEL` above: `debug`, `info`, `warn` (or `warning`), or `error`, case-insensitive, defaulting to `info`. |
| `MHVTL_CHANGER_DEV` / `MHVTL_DRIVE0_DEV` / `MHVTL_DRIVE1_DEV` | only for dry-run submissions | Same mhvtl device nodes `tapectl run --dry-run` requires (see above). `POST /api/runs` with `"dryRun": true` fails closed with `400` unless all three are set on `cmd/web`'s own environment — a dry-run submitted through the browser never falls back to real hardware. |
| `LIBRARY_CHANGER` / `LIBRARY_DRIVES` / `DELIVERY_WEBHOOK_URL` | no | Deploy-owned library device targets and Discord webhook URL — a changer/drive path or a webhook URL is a property of the deployment/host, not something an operator re-types (or mis-types) on every run, so the guided config form (the "Start new run" page's Form mode) **has no field for them at all**. `LIBRARY_CHANGER` is the changer device (e.g. `/dev/sch0`); `LIBRARY_DRIVES` is a comma-separated list of tape-drive devices (e.g. `/dev/nst0,/dev/nst1`); `DELIVERY_WEBHOOK_URL` is the Discord report webhook. Form mode fills these into the submitted run config from deploy config, so it still carries them (the run config stays the single source of truth) — validation (`library.changer` non-empty, ≥1 drive) is unchanged and enforced before submit. Each is also **enforced server-side**: `cmd/web` overwrites that field on every submitted run config with the deploy-owned value before the run is started (`pkg/runsapi` `applyDeployConfig`), so no submission path — Form mode, JSON / paste mode, or a direct `POST /api/runs` — can target a changer, drive, or webhook the host does not own. A field left unset here has no form control, so a Form-mode run leaves it empty and the Review step reports the corresponding validation error rather than the UI guessing a default; only an unset field can be supplied per run via JSON / paste mode. Surfaced to the SPA by the session-gated `GET /api/config/ui` route (see below). These are the real-hardware analogue of the dry-run-only `MHVTL_*` vars above (which `runsubmit.ApplyDryRun` still overrides for a dry-run — the mhvtl override runs after this one, so a dry run still redirects to the virtual library and never touches real hardware). |
| `OPTICAL_BURNER_DRIVES` | no | Deploy-owned **optical burner device paths** — the delivery analogue of `LIBRARY_DRIVES` above: a burner device path (e.g. `/dev/sr0`) is a property of the deployment/host, not a per-run choice, so the guided config form sources it read-only and the operator only toggles optical burn on/off and sets the copy count per run. A comma-separated list (e.g. `/dev/sr0,/dev/sr1`). Enforced **server-side**, but only when a submitted run actually enables optical burn (carries a `delivery.opticalBurn` block): `cmd/web` overwrites `delivery.opticalBurn.drives` with the deploy-owned list before the run is started (`pkg/runsapi` `applyDeployConfig`), so no submission path can burn on a device the host does not own. A run with no `opticalBurn` block (burn off) never gains a spurious one. Unset means a run enabling optical burn leaves the drives empty and the Review step reports the corresponding validation error rather than the UI guessing a default (JSON / paste mode remains the escape hatch). Surfaced to the SPA by the session-gated `GET /api/config/ui` route (see below). |
| `LIBRARY_SLOT_COUNT` / `LIBRARY_CLEANING_SLOTS` / `LIBRARY_IO_STATION_SLOTS` | no | Deploy-owned **library topology** the guided config form's Library section uses to render the blank/write-target **slot-grid picker** bounded to the deployment's real library, rather than a free-form list of arbitrary slot numbers — the physical library's slot layout is a property of the deployment, not a per-run choice. `LIBRARY_SLOT_COUNT` is the number of physical storage slots (a single integer); the picker numbers them `1..LIBRARY_SLOT_COUNT`. `LIBRARY_CLEANING_SLOTS` and `LIBRARY_IO_STATION_SLOTS` are comma-separated slot numbers reserved for cleaning cartridges and the I/O station (import/export / mail slot), respectively (e.g. `45` and `46,47`); the picker renders them non-selectable so a run can never target them. The operator's per-run **selection** of blank slots (`library.blankSlots`) is still made in the form — the topology only bounds it. Unset/blank `LIBRARY_SLOT_COUNT` (or a non-numeric value) makes the picker show a "not configured" state; the operator can still set `library.blankSlots` per run via JSON / paste mode. Surfaced to the SPA by the session-gated `GET /api/config/ui` route (see below). This static topology does **not** include live per-slot occupancy (which barcode is in which slot right now) — that needs live changer element-status on the storage host, out of scope here. |
| `VICTORIAMETRICS_URL` | no | Base URL of a VictoriaMetrics instance scraping the workers' `METRICS_ADDR` endpoints (e.g. `http://127.0.0.1:8428`), backing the live drive metrics endpoints — see [Live drive metrics (VictoriaMetrics)](#live-drive-metrics-victoriametrics) above. Unset disables live drive metrics entirely: both endpoints return a stable `503` rather than falling back to any other data source. |
| `VICTORIALOGS_URL` | no | Base URL of a VictoriaLogs instance (e.g. `http://victorialogs:9428`) that an external log collector (outside this repo's scope) ships worker `slog` JSON stdout into. Backs `GET /api/runs/{runID}/logs` (see below). Unset means logs are simply unavailable — `cmd/web` still starts and runs normally, the log panel just shows its explicit "unavailable" state. |
| `VICTORIALOGS_STREAM_FILTER` | no | A LogsQL filter fragment ANDed onto every log query `GET /api/runs/{runID}/logs` issues (e.g. to scope queries to one tenant/stream in a shared VictoriaLogs deployment). Defaults to `*` (match everything) when unset — the right default for a VictoriaLogs instance dedicated to this deployment. |
| `VICTORIALOGS_FIELD_PREFIX` | no | A field-name prefix for the worker's `slog` fields (`RunID`, `level`, `msg`, `Error`/`error`), for collectors that nest them under a prefix instead of shipping them as top-level VictoriaLogs fields — set it when a fluentbit/fluentd `kubernetes` filter runs with `Merge_Log On` + `Merge_Log_Key <key>` (each parsed JSON key lands under `<key>.<name>`, and `_msg` holds the raw JSON line). The value is the merge key plus a trailing dot, e.g. `log_fields.`; the log query then filters on `"log_fields.RunID"` and the message text comes from `log_fields.msg` (falling back to `_msg` only when absent). Empty (default) = the top-level-field shape the dev stack's `vector` shipper produces (`_msg_field=msg`) — full backward compatibility. VictoriaLogs-owned fields (`_time`, `_stream`) and `VICTORIALOGS_STREAM_FILTER` are never prefixed. |
| `OIDC_ISSUER_URL` | yes | The OIDC identity provider's issuer URL, used for discovery (`GET {OIDC_ISSUER_URL}/.well-known/openid-configuration`). Any standards-compliant provider works (Keycloak, Authentik, Dex, ...) — `cmd/web` contains no IdP-specific code. |
| `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | yes | This app's confidential-client credentials at the provider above. |
| `OIDC_REDIRECT_URL` | yes | This app's OIDC callback URL, exactly as registered with the provider (e.g. `https://tape-archiver.example.com/auth/callback`) — see [OIDC authentication](#oidc-authentication-cmdweb) below. |
| `WEB_SESSION_KEY` | yes | A base64-encoded 32-byte AES-256 key (e.g. the output of `openssl rand -base64 32`) encrypting the session and login-state cookies. `cmd/web` holds no server-side session store (`docs/web-ui-design.md` §3), so losing or rotating this key just signs every operator out — nothing else depends on it. |
| `WEB_FOOTER_HOST` | no | An optional deploy-time label (e.g. a host or deployment name, `homelab-01`) shown after the build version in the UI's footer line (`tape-archiver <version> · <label>`, on the login page and in the sidebar). When unset, the footer shows only the version — the label segment is omitted entirely, not left blank. The version segment itself always comes from the binary's own embedded VCS build info (`internal/buildinfo.ToolVersion`), not from any environment variable. Served (with the version) by the ungated `GET /api/build-info` route — do not put anything sensitive in it. |

`cmd/web` fails to start if it cannot reach Temporal (same startup health check as
`worker`/`tapectl` — `pkg/temporalclient.New` — since a run browser that cannot reach
Temporal cannot do anything useful), or if the OIDC configuration above is incomplete or
malformed (`pkg/webauth.New`, including OIDC discovery against `OIDC_ISSUER_URL`) — every
data-bearing `/api/*` route is gated behind a valid session, so a working OIDC setup is
not optional.
`/readyz` subsequently reflects Temporal connectivity going forward, e.g. if Temporal
becomes unreachable after startup.

#### Access log

`cmd/web` emits one structured (`slog` JSON, to stderr) access-log record per completed
HTTP request — the SPA, its assets, `/api/*`, and the `/auth/*` routes alike — with
`msg` `web: request`. Each record carries the request `method`, the URL `path`, the
response `status` and `bytes`, the elapsed `duration_ms`, the client `remote` address,
and — when the request carried a valid session cookie — the authenticated operator's
OIDC `subject` as `user`. The record's level tracks the status so failures stay visible
even at a raised `LOG_LEVEL`: `5xx` logs at `error`, `4xx` at `warn`, everything else at
`info` (so `LOG_LEVEL=warn` keeps only failed requests).

A failing request also carries a **reason** explaining *why* it ended as it did, and any
record carrying such a reason is raised to at least `warn` (so it stays visible at
`LOG_LEVEL=warn` even when its status alone would be `info`):

- A gated `/api/*` request rejected by the session middleware carries `deny_reason` —
  `no session cookie` / `invalid or expired session cookie` for a `401`, or
  `cross-site mutation blocked (CSRF)` for a `403`.
- A **failed login** carries `auth_error` naming the stage that failed. This is the
  field to look at for the SPA's "authenticated but isn't authorized for this archive"
  message: `cmd/web` applies **no authorization of its own** (any authenticated user is
  allowed), so that message means the **identity provider itself** denied the login —
  the callback arrives at `/auth/callback?error=...`. That record carries
  `auth_error: "identity provider returned an error"` plus the provider's own
  `idp_error` (e.g. `access_denied`) and `idp_error_description` — look at the IdP's
  side (an app-assignment/consent/policy denial), not tape-archiver. Note the failing
  callback is served as a `302` redirect, so filtering by error *status* alone will not
  surface it — filter on the presence of `auth_error` (or the `warn` level). Other
  `auth_error` values (`state mismatch (stale login attempt or CSRF)`, `authorization
  code exchange failed`, `id_token verification failed`, `id_token nonce mismatch`, …)
  cover the remaining callback-rejection stages, some with an `error` field carrying the
  underlying failure.

Two things are deliberately
excluded: the **query string** is never logged (only `path`), so the OIDC authorization
`code`/`state` on `/auth/callback` never reach the log — run IDs, which live in the path
(`/api/runs/{runID}`), are logged, as an access log is meant to record which resource was
touched; and no request/response **body, headers, or cookies** are logged. `remote` is
the first `X-Forwarded-For` entry when present (the original client behind the
TLS-terminating proxy, trusted on the same basis as `X-Forwarded-Proto` for the `Secure`
cookie flag), falling back to the direct peer address otherwise.

#### `GET /api/runs` and `GET /api/runs/{runID}`

Both are read-only views over Temporal visibility and the backup workflow's own query
handlers — there is no UI-owned store (SPEC §4.2). `GET /api/runs` lists every execution
of the singleton `backup` workflow ID, newest first; `GET /api/runs/{runID}` (Temporal's
run ID, which disambiguates individual executions of that one workflow ID) additionally
reports the last completed phase (`lastCompletedPhase` query) and, since the `workflows/
backup` `currentPause` query landed, whether the run is currently paused waiting on an
operator and why (`currentPause`, an object): `{"kind": "eject"|"write-failure"|"burn"|"",
"phase": "...", "affectedTapes": [...], "reloadSlots": [...], "awaitingExport": N,
"devices": [...], "errorSummary": "..."}` — `kind` is `""` when the run is not paused, and
every other field is populated only where it applies to that pause kind (e.g. `phase`/
`reloadSlots` for a Load/Write failure, `awaitingExport` for an Eject pause, `devices` for
a Burn pause). An unknown but well-formed run ID is `404`; a malformed one (Temporal run
IDs are UUIDs) is `400`. Both, like every `/api/*` route, require an authenticated session
(see below) — an unauthenticated request gets `401`, not `404`/`400`.

### OIDC authentication (`cmd/web`)

Every `/api/*` route (except the ungated `/api/build-info` below) is gated behind an
OIDC authorization-code-flow session (`pkg/webauth`), authentication only — any
authenticated user is authorized; there is no role/permission model. Page routes serve
the SPA itself unconditionally (the bundle carries no data — everything sensitive is
behind the gated API), and the SPA shows a styled login page until a session exists.
The provider is entirely configured via
`OIDC_ISSUER_URL`/`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET`/`OIDC_REDIRECT_URL`
above (OIDC discovery + a standard authorization-code exchange), so any compliant identity
provider works without code changes.

Routes:

| Route | Method | Gated? | Purpose |
|-------|--------|--------|---------|
| `/auth/login` | `GET` | no | Starts the flow: sets a short-lived (10 minute), encrypted login-state cookie (CSRF state, OIDC nonce, PKCE verifier, and the originally requested path) and redirects to the provider's authorization endpoint. An optional `?redirect=/some/path` query parameter controls where the browser lands after a successful login (must be a same-origin absolute path; anything else is ignored in favor of `/`). The UI's login page triggers this route when its sign-in control is activated. |
| `/auth/callback` | `GET` | no | The provider's redirect target: validates the CSRF state, exchanges the authorization code (with the PKCE verifier from the state cookie), verifies the returned ID token's signature/issuer/audience/expiry/nonce, sets the session cookie, and redirects into the app. On any failure it redirects to the UI's login page with an error the page renders — `/login?error=denied` when the provider itself reported a denial (its `error` callback parameter, e.g. the account is not authorized), `/login?error=expired` for everything else (a missing/expired/tampered login-state cookie, CSRF state mismatch, a failed code exchange, an invalid/expired ID token, a nonce mismatch) — never a bare HTTP error page. On a denial the provider's own `error_description` is carried through (`&error_description=`, sanitized) and shown on the login page, attributed to the provider, so a provider-side refusal is not opaque. The original destination is carried along as `redirect=` when known, so a successful retry still lands where the operator was headed. |
| `/auth/logout` | `GET` | no | Clears the session cookie and redirects to `/` (which, now unauthenticated, renders the UI's login page). Logging out an already-logged-out session is a no-op, not an error. |
| `/api/build-info` | `GET` | no | The build version and optional footer label: `{"version": "...", "footerHost": "..."}` (`footerHost` omitted when `WEB_FOOTER_HOST` is unset). Ungated deliberately — the login page's footer renders it before any session exists, and a build version/deploy label is not sensitive. |
| `/api/me` | `GET` | yes | The authenticated identity: `{"subject": "...", "email": "...", "name": "..."}`, taken from the ID token's `sub`/`email`/`name` claims (`name` falls back to `preferred_username` when absent; `email`/`name` are omitted from the response when the provider does not supply them). |
| `/api/config/ui` | `GET` | yes | Server-provided deploy config the SPA needs: `{"temporalUiBaseUrl": "...", "temporalNamespace": "...", "library": {"changer": "...", "drives": ["..."], "slotCount": 0, "cleaningSlots": [], "ioStationSlots": []}, "delivery": {"webhookConfigured": false, "opticalBurnDrives": ["..."]}}`. `temporalUiBaseUrl` is empty (and the run overview's Temporal-workflow link omitted) unless `TEMPORAL_UI_URL` is set; `temporalNamespace` is the namespace resolved from the Temporal client config profile. `library.changer`/`library.drives` are the deploy-owned device targets the guided config form fills into a run (from `LIBRARY_CHANGER`/`LIBRARY_DRIVES`), each empty when unconfigured. `delivery.webhookConfigured` reports only *whether* a Discord webhook is set (from `DELIVERY_WEBHOOK_URL`) — never the URL itself, which is a credential: `cmd/web` applies the deployment's own webhook to every submitted run server-side, so the SPA never needs (and is never sent) the value. `delivery.opticalBurnDrives` is the deploy-owned optical burner device paths (from `OPTICAL_BURNER_DRIVES`), `[]` when unconfigured, shown read-only in the guided form when a run enables optical burn. `library.slotCount`/`library.cleaningSlots`/`library.ioStationSlots` are the deploy-owned library topology (from `LIBRARY_SLOT_COUNT`/`LIBRARY_CLEANING_SLOTS`/`LIBRARY_IO_STATION_SLOTS`) driving the guided form's slot-grid picker — `slotCount` is `0` and the slot arrays `[]` when no topology is declared. Carries no per-run or sensitive data. |

Gating split: an unauthenticated request under `/api/` gets `401` with a JSON
`{"error": "..."}` body (a `fetch()`/XHR caller cannot usefully follow an HTML redirect);
an unauthenticated request to any other route (the SPA at `/`, or any client-side route
under it) is served the SPA normally — the app itself detects the missing session (via
`GET /api/me`) and shows its styled login page (route `/login`), whose sign-in control
starts the flow via `/auth/login`. The SPA bundle carries no secrets; all data lives
behind the `401`-gated `/api/*` routes. A tampered or expired session cookie is rejected
exactly like a missing one — never a `500`.

The session is a stateless, encrypted, tamper-evident cookie (AES-256-GCM, keyed by
`WEB_SESSION_KEY`), not a server-side store, so `cmd/web` stays fully stateless and can
scale or restart freely (`docs/web-ui-design.md` §3; SPEC §4.2). A session's lifetime
follows the ID token's `exp` claim, capped at 24 hours even if the provider issues a
longer-lived token.

#### `POST /api/runs`

Submits a backup run — the browser's front door to the same submission path
`tapectl run [--dry-run]` uses (`pkg/runsubmit`, shared by both so they can never
drift). The request body is `{"config": <run-config JSON>, "dryRun": <bool>}`; `config`
is validated with the same `internal/config` rules `tapectl` applies (unknown fields
rejected, all cross-field invariants checked) before Temporal is ever contacted. Like
every mutating `/api/*` route, a state-changing request a browser labels cross-site
(`Sec-Fetch-Site: cross-site`, or an `Origin` whose host does not match the request's) is
refused with `403` — defence in depth behind the session cookie's `SameSite=Lax`;
same-origin browser requests and non-browser API clients (which send neither header) are
unaffected. Any
deploy-owned library device or Discord webhook the host configured
(`LIBRARY_CHANGER`/`LIBRARY_DRIVES`/`DELIVERY_WEBHOOK_URL`, see
[Web UI environment variables](#web-ui-environment-variables-cmdweb) above) is then
**overwritten** onto the submitted config — so a submission cannot target a changer,
drive, or webhook the deployment does not own, whether it came from the guided form,
JSON / paste mode, or a direct `POST`. The deploy-owned optical burner drives
(`OPTICAL_BURNER_DRIVES`) are overwritten the same way, but only when the submitted run
actually enables optical burn (carries a `delivery.opticalBurn` block) — a burn-off run
never gains a spurious one. A **production (non-dry-run) run additionally requires the
deployment to own the devices it will touch — and the delivery webhook**: if
`LIBRARY_CHANGER` or `LIBRARY_DRIVES` is unset (or `OPTICAL_BURNER_DRIVES` is unset for a
run that enables optical burn, or `DELIVERY_WEBHOOK_URL` is unset for a run that submits a
`delivery.webhookUrl`), the submission is refused with `400` and a message naming the
missing variable — a real run must never be aimed at a client-supplied device node the
host has not declared it owns, nor deliver its report (which embeds the age escrow private
key, SPEC §7) to a client-supplied webhook. Configure the deployment's devices/webhook, or
submit as a dry-run. When `dryRun` is `true`, the
library device targets are redirected to the `mhvtl` nodes named by
`MHVTL_CHANGER_DEV`/`MHVTL_DRIVE0_DEV`/`MHVTL_DRIVE1_DEV` and optical burning is
disabled — identical to `tapectl run --dry-run`; this dry-run redirect runs after the
deploy-owned overwrite, so a dry run always lands on the virtual library and is therefore
exempt from the device-ownership requirement above.

The dry-run redirect covers **hardware only** — it does not clear the Discord webhook. A
dry-run's Deliver phase still posts its run report (and any failure alert) to the
configured `DELIVERY_WEBHOOK_URL` channel, exactly as a production run would; this is
intended, so a dry-run exercises the full delivery pipeline (notifications included)
against whatever channel the deployment points at. Point `DELIVERY_WEBHOOK_URL` at a test
channel if dry-run notifications should not reach the production one.

On success the response is `201 Created` with `{"workflowId": "backup", "runId": "..."}`
(a `Location: /api/runs/{runId}` header points at the new run's detail endpoint). An
invalid config, malformed request body, a production run whose deploy-owned devices are
not configured, or a dry-run with the mhvtl variables unset is
`400` before any Temporal RPC is made. Because backup runs are a singleton (SPEC §4.2,
workflow ID always `backup`), a submission while one is already in progress is refused
with `409 Conflict` rather than being queued or silently replacing the in-flight run —
the same guard `tapectl run`'s `WorkflowIDConflictPolicy` enforces.

#### `POST /api/runs/{runID}/resume` and `POST /api/runs/{runID}/abort`

Send the backup workflow's existing operator signals — `operatorResume` /
`operatorAbort` (`workflows/backup/contract.go`) — the same two signals `tapectl
resume`/`tapectl abort` send. Unlike the CLI, which signals unconditionally (a human
operator has just watched the pause happen), these routes first check the run's current
pause state (`currentPause` query, the same one `GET /api/runs/{runID}` reports) and
refuse to send a signal a running workflow would only buffer and potentially misapply to
a later, unrelated pause:

- If the run is not currently paused, both routes return `409 Conflict` without sending
  anything.
- `POST /api/runs/{runID}/abort` additionally returns `409 Conflict` for an Eject pause
  (`currentPause.kind == "eject"`): every tape is already safely written by the time that
  pause fires, so the workflow's Eject wait never listens for the abort signal in the
  first place — only resume applies there.
- An unknown run ID is `404`; both, like every `/api/*` route, require an authenticated
  session.

On success the response is `202 Accepted` with `{"status": "resume signal sent"}` (or
`"abort signal sent"`) — confirmation that the signal was sent, not that the run has
necessarily processed it yet; poll `GET /api/runs/{runID}` or watch `GET
/api/events/runs/{runID}` below for the pause actually clearing.

#### `GET /api/events/runs/{runID}` (live monitoring)

A `text/event-stream` (Server-Sent Events) view over the same data `GET
/api/runs/{runID}` serves: `cmd/web` polls Temporal (`DescribeWorkflowExecution` +
`lastCompletedPhase` + `currentPause`) at a short, fixed server-side interval and pushes
an `update` event — identical in shape to `GET /api/runs/{runID}`'s response body
(`{"workflowId", "runId", "status", "startTime", "closeTime", "lastCompletedPhase",
"currentPause"}`) — only when the polled status, phase, or pause state actually changed
since the last event, so a quiescent run does not produce a stream of redundant events.
An operator-in-the-loop pause starting or clearing (e.g. after a resume/abort sent via
the routes above) is exactly the kind of change that can happen with neither `status` nor
`lastCompletedPhase` moving, so `currentPause` is compared alongside them — a client
watching the live stream sees a pause (and its clearing) without a manual refresh. Once
the run reaches a terminal status (anything other than `RUNNING`), the server sends one
final `update` followed by a `done` event (same body) and then closes the connection
itself — the stream never polls a finished run forever, and a client does not need to
reconnect to learn that.

The very first poll decides whether the response becomes a stream at all: an unknown run
ID is a normal `404` JSON error (not a `200` stream that immediately fails), a malformed
one is `400`, matching `GET /api/runs/{runID}`'s own error mapping exactly. Like every
other `/api/*` route, an unauthenticated request is `401`, not a `200` stream — session
cookies are sent automatically by a same-origin `EventSource`, so this requires no special
client-side auth handling beyond what any other page fetch already needs. The connection
closes promptly if the client disconnects; there is no server-side per-connection state
left behind either way.

The web UI's run-detail view (`RunDetail.tsx`) is the primary consumer, reachable from
the submit form's success state.

#### History-derived run endpoints

`GET /api/runs/{runID}/phases`, `GET /api/runs/{runID}/config`,
`GET /api/runs/{runID}/tapes`, `GET /api/runs/{runID}/delivery`, and `GET /api/tapes`
reconstruct richer per-run data — a full phase timeline, the originally submitted run
config, per-run/aggregate tape outcomes, and the report's Discord deep-link — entirely
on demand from the run's raw Temporal workflow event history
(`GetWorkflowHistory`). There is no persistent catalog and no cross-run state (SPEC
§4.2): everything below is derived from Temporal's own records at request time, and a
run whose history has aged out of Temporal's retention window is therefore genuinely
no longer reconstructable (reported explicitly, see the error mapping below). The
history is parsed as raw events, never replayed against workflow code, so runs
recorded by older deployed versions of the workflow — or non-backup stub workflows
sharing the `backup` workflow ID (e.g. from tests) — degrade to partial data rather
than erroring.

All three per-run endpoints share one error mapping, extending `GET
/api/runs/{runID}`'s: a malformed run ID (Temporal run IDs are UUIDs) is `400`; a run
ID Temporal has no record of at all is `404`; and — distinct from both — a run that
verifiably existed (it still appears in Temporal visibility, the same index `GET
/api/runs` lists) but whose event history has aged out of the retention window is
`410 Gone`, with a message saying the history can no longer be reconstructed. Like
every `/api/*` route, an unauthenticated request is `401`.

`GET /api/runs/{runID}/phases` returns `{"runId": "...", "phases": [...]}` with all 11
pipeline phases (SPEC §4.3) in pipeline order — `Resolve`, `Prepare`, `Pack`,
`Generate PAR2`, `Verify`, `Load`, `Write`, `Eject`, `Report`, `Burn`, `Deliver` —
each `{"name", "status", "startTime", "endTime", "facts", "error"}`. `status` is one
of `pending`, `active` (in progress — including while paused for an operator;
`currentPause` on `GET /api/runs/{runID}` reports the pause itself), `completed`, or
`failed`. `startTime`/`endTime` bracket the phase's activity window and are omitted
where unknown (a pending phase, or a phase that completed as a no-op — e.g. `Burn` on
a run without `delivery.opticalBurn`).

Because the Load/Write/Eject tape path interleaves per drive-set (SPEC §4.3 phases
6–8), a later phase can hold real activity from an earlier drive-set while an earlier
phase is still running — or has failed — for a later set. Statuses therefore follow
the run's *pipeline frontier*, not raw "does a later phase have activity": `failed`
marks exactly the phase the run's own failure record names, regardless of any
later-phase activity earlier drive-sets left behind; phases before it are
`completed`; phases after it are `pending` even when they hold an earlier set's
partial activity (on a run failed at a later set's Write, set 1's already-ejected
tapes do not make `Eject` "completed" — the per-set reality stays visible through the
tape-outcome endpoints below). On a still-running run, a later phase holding an
earlier drive-set's activity reads `active` (the interleaved tape path is in progress
as a unit), never prematurely `completed`. A phase containing individually
failed-and-retried work the run moved past (a Load/Write-failure pause resumed onto
fresh blanks) is `completed`, not `failed`, with `error` carrying the failure text
only on the `failed` phase. `facts` is a list of `{"key", "label", "value"}` observable facts
(each optionally carrying a `title` — an exact/expanded form of `value` for a client
to surface as hover text, e.g. the precise byte count behind Prepare's humanized
`stagedBytes` `"5.6 GB"`) recovered from the phase's own activity payloads where
available — e.g. Resolve's
`archives`, Prepare's `archivesStaged`/`stagedBytes`, Pack's `logicalTapes`/`copies`,
Generate PAR2's `recoverySets`, Verify's `filesVerified` (`"N/N"`), Load's
`tapesLoaded`, Write's `tapesWritten` (and `tapesFailed` when any tape failed),
Eject's `tapesExported`, Report's `reportBuilt`/`isoBuilt`, Burn's `discsBurned` (or
`opticalBurn: disabled`), Deliver's `delivered`. Facts are best-effort: a phase still
running, or recorded by an older workflow version whose payload shapes differ, simply
reports fewer facts.

`GET /api/runs/{runID}/config` returns `{"runId": "...", "config": {...}}` — the run
configuration recovered from the workflow's own start input, i.e. exactly what was
submitted to Temporal for that run (for a dry-run, that is the post-override config
the run actually executed with: mhvtl device nodes, optical burning removed). Two
deliberate exceptions — the two credential-bearing fields in the whole config, per a
field-by-field sweep — are replaced with `"***redacted***"` and never leave the
server: `encryption.identity` (the age *private* decryption key, escrowed only into
the printed report and recovery ISO — see [Encryption](#encryption)) and
`delivery.webhookUrl` (a Discord webhook URL embeds its auth token in the path, so
the URL alone lets anyone post to the channel). `encryption.recipients` (public keys)
and every other field are returned as-is. A history whose start input cannot be
decoded as a run config (a foreign/stub workflow under the `backup` workflow ID) is
`422`, not a `500`.

`GET /api/runs/{runID}/tapes` returns `{"runId": "...", "tapes": [...]}` — every
physical tape the run loaded, each
`{"barcode", "tapeIndex", "copyIndex", "driveIndex", "slot", "result", "error",
"overwroteNonBlank", "writeHealth"}`. `result` is `written` (formatted, written, and
finalized successfully), `failed` (the format/write/finalize pipeline failed — `error`
carries the failure text), or `loaded` (loaded but the run has not finished it yet —
still in progress, or it ended first). A Load/Write-failure retry loads a fresh blank
(a different physical tape), so the failed tape and its replacement each appear as
their own entry (SPEC §4.3's bounded blast radius, visible per tape). `writeHealth`,
when present, is the tape's observational write-health verdict (SPEC §14):
`{"measured", "throughputMBps", "floorMBps", "floorKnown", "belowFloor",
"repositions", "repositionsMeasured", "tapeAlertFlags", "healthy"}` — `measured:
false` means the measurement could not be taken at all (the run still succeeds; every
other field is then a zero placeholder), so an unmeasured tape is distinguishable
from one measured and found unhealthy (`healthy` is `false` in both cases).

`GET /api/runs/{runID}/delivery` returns `{"runId": "...", "messageUrl": "..."}` —
the Discord jump-to-message deep-link
(`https://discord.com/channels/{guild}/{channel}/{message}`) for the run's posted PDF
report, reconstructed from the `Deliver` activity's recorded result (the message's
guild/channel/message identity, captured when the report is uploaded with
`?wait=true`). `messageUrl` is `""` — and the run overview shows no **Discord report ↗**
link — when the run delivered no report (delivery disabled or not yet reached), the
delivery failed, or the guild could not be resolved. Needs no configuration: the
identity travels in the run's own history, not an external catalog (SPEC §4.2).

`GET /api/tapes` returns `{"tapes": [...], "runErrors": [...]}` — the tape outcomes
above aggregated across the most recent runs still in Temporal visibility, newest run
first, each entry additionally carrying `{"runId", "runStartTime", "runStatus"}` so
every tape is attributable back to its run (this drives the tapes page and the
dashboard's library card). Reconstructing each run costs a full history fetch, so an
optional `limit` query parameter bounds how many of the newest runs are
reconstructed: default `50`, capped at `1000` (the visibility page size); a
non-positive or non-numeric value is `400`. The listing degrades per run within that
limit and never fails as a whole because one run is unreconstructable: a run whose
history is gone (aged out) or unreadable contributes a `{"runId", "error"}` entry to
`runErrors` instead, while every other run's tapes are still listed. Runs older than
Temporal's retention are absent entirely — their tape contents genuinely cannot be
recovered without a catalog, which is by design (SPEC §4.2).

### Live drive metrics (VictoriaMetrics)

`GET /api/runs/{runID}/metrics/drives` and `GET
/api/runs/{runID}/metrics/drives/{barcode}/history` (issue #275) are a thin, read-only
proxy over VictoriaMetrics for the write-health gauges the data worker already exports
(`workflows/backup/writehealth.go`'s `tape_archiver_write_throughput_mbps`/
`repositions`/`tapealert_flags`/`below_floor`, all labeled `barcode`) — no new metric
instrumentation is added anywhere. `cmd/web` never becomes an open PromQL proxy: every
query is built server-side from a fixed metric-name allowlist and a barcode the
requested run's own Temporal history actually loaded, and the sparkline range/step are
fixed server-side constants — a client can never supply raw PromQL, an arbitrary
metric, a foreign barcode, or an arbitrary time range. This intentionally covers only
the current run's live drive view; a historical/long-range metrics explorer is out of
scope (a Grafana-style tool is the right fit for that, not this API).

VictoriaMetrics is optional observability. `VICTORIAMETRICS_URL` (e.g.
`http://127.0.0.1:8428`) configures it; both endpoints return `503` with a stable
`{"error": "..."}` body whenever it is unset, and the same `503` (never `500`) for any
failure actually reaching or parsing a VictoriaMetrics response — a misbehaving or
unreachable metrics backend never makes the rest of the run detail API look broken.
The frontend polls these endpoints every 5 seconds (a plain interval, not
Server-Sent Events — this data is optional best-effort observability that may be
entirely unconfigured, unlike run status/phase) rather than opening a second stream —
but only while the run is still in progress. Once a run reaches a terminal status, the
UI stops querying VictoriaMetrics for it entirely and instead renders the final
per-tape write-health from the run's own history (`GET /api/runs/{runID}/tapes`'s
`writeHealth`, labeled as final measurements): a closed run must not poll forever, and
VictoriaMetrics samples are only attributable to a run by which barcode is loaded
*right now* — a barcode reused by a later run would otherwise have that later run's
readings shown on an old run's page.

`GET /api/runs/{runID}/metrics/drives` returns `{"runId": "...", "drives": [...]}` —
one entry per tape barcode the run has loaded (same set `GET /api/runs/{runID}/tapes`
derives), each `{"barcode", "tapeIndex", "copyIndex", "driveIndex", "result",
"hasData", "throughputMBps", "repositions", "tapeAlertFlagCount", "belowFloor",
"floorMBps", "floorKnown"}`. `hasData: false` means VictoriaMetrics has no sample for
this barcode yet (not yet measured — still writing, or the run never reached
`MeasureWriteHealth` for it), distinguishable from an actual zero/false reading.
`floorMBps`/`floorKnown` are the tape's speed-matching floor from the run's own history
(the floor is a static, generation-derived constant, not its own gauge). An empty
`drives` list (`200`, not an error) means the run has not loaded any tape yet.

`GET /api/runs/{runID}/metrics/drives/{barcode}/history` returns `{"runId": "...",
"barcode": "...", "metric": "...", "points": [{"time", "value"}, ...]}` — a fixed
8-point, 90-second-step VictoriaMetrics range query (the design's 8-bar write-rate
sparkline) for one metric of one tape. `barcode` must be one this run actually loaded
(`404` otherwise). The optional `metric` query parameter selects among `throughput`
(the default), `repositions`, `tapealerts`, or `belowfloor` — any other value is `400`.

#### `GET /api/runs/{runID}/logs` (log panel, issue #274)

A read-only proxy over an external VictoriaLogs instance (`VICTORIALOGS_URL`,
`VICTORIALOGS_STREAM_FILTER` — see the environment variable table above), never a raw
LogsQL passthrough: `cmd/web` builds the query itself from validated parameters. Returns
`{"runId", "phase", "lines": [{"time", "level", "message", "error"}, ...], "live"}` —
`lines` are the matched log lines, oldest first; `live` is `true` while more lines can
still arrive for the requested window (the run, or the given phase, has not finished yet).
`error` is present only when a line carried a structured error field — a failing/retrying
activity, for example, logs a terse `message` ("Activity error.") and puts the actual
cause in a field (the Temporal SDK's `Error`, or this repo's slog `error`), which this
projects out so the log panel can show it without every call site inlining the cause into
its message text.

Query parameters:

- `phase` (optional) — one of the 11 pipeline phase names (`GET
  /api/runs/{runID}/phases`' `name` values); an unknown value is `400`. Omitted, the
  window is the whole run (its start to its close time, or now if still running).
  Given, only lines emitted during that phase's own activities are returned (the
  same per-activity attribution `GET /api/runs/{runID}/phases` uses) — because the
  Load/Write/Eject tape path interleaves per drive-set and retries after operator
  pauses (SPEC §4.3 phases 6–8), one phase's activities are not contiguous in time,
  and a naive earliest-to-latest window would include another phase's lines. A phase
  that has not started yet returns `{"lines": [], "live": false}`, a normal empty
  result, not the "unavailable" state below.
- `since` (optional) — an RFC3339 timestamp; only lines at or after it (inclusive)
  are returned. Intended for a client polling for new lines (e.g. `since=` the last
  line's own `time`) without re-fetching the whole window every time — this is how
  the web UI's log panel gets new lines to appear without a full page reload while
  `live` is `true`, deliberately by polling rather than Server-Sent Events: unlike
  `GET /api/events/runs/{runID}`, a browser `EventSource` has no way to expose a
  failed connection's HTTP status to JS, which would make the "unavailable" state
  below indistinguishable from an ordinary transient drop. The bound is inclusive
  on purpose: log shipping into VictoriaLogs is asynchronous and batched, so with an
  exclusive bound a poll could permanently miss lines sharing its `since` timestamp
  that had not been ingested yet — a polling caller must instead deduplicate the
  re-sent boundary lines (the web UI's log panel dedups by time + level + message).
- There is no client-facing result-count limit — the server always caps a single
  response at a fixed number of lines (currently 5000) regardless of how many
  actually matched, so an operator glancing at recent activity gets a bounded
  response without needing to reason about paging.

`runID` must be a well-formed UUID (Temporal run IDs always are) or the request is
`400` before any Temporal or VictoriaLogs call is made — stricter than every other
`/api/runs/{runID}/*` route, because unlike those this one interpolates `runID` into a
query-language string rather than only ever passing it as an opaque RPC argument.
Unknown/aged-out run IDs use the same `404`/`410` classification as the history-derived
endpoints above.

**Collector field naming (`VICTORIALOGS_FIELD_PREFIX`).** By default this endpoint
expects the worker's `slog` keys as top-level VictoriaLogs fields (`RunID`, `level`,
`Error`/`error`) with the human message in `_msg` — the shape the dev stack's `vector`
shipper produces (`_msg_field=msg`). Real-world Kubernetes log pipelines often differ: a
fluentbit/fluentd `kubernetes` filter with `Merge_Log On` + `Merge_Log_Key <key>` parses
each JSON log line and nests every key under `<key>.<name>` (VictoriaLogs flattens the
nested object to dotted field names), leaving `_msg` holding the raw JSON line rather than
the message. Against such a pipeline the default `RunID:=` filter matches zero records and
the panel is permanently empty. Set `VICTORIALOGS_FIELD_PREFIX` to the merge key plus a
trailing dot (e.g. `log_fields.` for `Merge_Log_Key log_fields`) to shift every
worker-originated field reference to the collector's naming: the query filters on
`"log_fields.RunID"` and the projected `level`/`error` and the message text are read from
`log_fields.level`/`log_fields.error`/`log_fields.msg` (the message falls back to `_msg`
only when `log_fields.msg` is absent). Fields VictoriaLogs itself owns (`_time`, `_stream`)
and `VICTORIALOGS_STREAM_FILTER` are never prefixed. Leaving it empty preserves exactly the
top-level-field behavior above.

When `VICTORIALOGS_URL` is unset, or is set but VictoriaLogs cannot be reached or
returns an error, the response is `503` with the same `{"error": "..."}` body shape
every other endpoint uses — an explicit, distinguishable "logs unavailable" state,
never a `500` and never a raw network error surfaced to the browser. The web UI's log
panel (`web/src/LogPanel.tsx`, used by `RunDetail.tsx` and reusable per-run or
per-phase) renders this as its own styled unavailable state, distinct from its loading
and empty (no lines yet) states.

#### `POST /api/age/keygen` (age keypair generation, issue #279)

Generates a fresh age **native post-quantum** keypair (hybrid ML-KEM-768 + X25519,
`age-keygen -pq` — the only recipient form the encryption pipeline accepts, see
[Encryption](#encryption)) by invoking the same bundled `age-keygen` binary that ships
on the recovery disc, so a generated identity is produced by the exact implementation a
future recoverer decrypts with. The request takes no body. On success the response is
`200` with:

```json
{ "recipient": "age1pq1…", "identity": "AGE-SECRET-KEY-PQ-1…" }
```

The returned `recipient` is re-derived from the generated identity via `age-keygen -y`
(the same derivation the Report phase uses to verify an escrowed identity), never
parsed from `age-keygen`'s own comment output, so the pair can never drift apart.

**The private `identity` exists only in this one response.** It is never logged, never
written to disk, and never persisted server-side in any form — `cmd/web` has no store
for it to land in, deliberately (SPEC §4.2) — and no endpoint can return it again. The
web UI's config page (its consumer) shows it exactly once with a copy control; once that
response is gone (page reload, navigation, a second generation), the identity is
unrecoverable through the app (though a completed run escrows it into its report and
recovery ISO — SPEC §7 — so it is not irrecoverable outright). The response carries
`Cache-Control: no-store` (and `Pragma: no-cache`) so no browser or intermediary cache
ever retains a copy. A keypair-generation
failure (e.g. the `age-keygen` binary missing from the image) is `500` with the usual
`{"error": "..."}` body — the error text never contains key material. Like every
`/api/*` route, an unauthenticated request is `401`.

#### `GET /api/config/schema` (run-config JSON Schema, issue #279)

Returns the committed run-config JSON Schema (`schemas/run-config.schema.json`,
embedded into the binary at build time) verbatim, as `application/json`. The web UI's
config page fetches it to validate Form-mode-built and JSON-mode-pasted configs
client-side against the exact same schema `make generate-schema` produces from
`internal/config`'s types — never a hand-duplicated copy that could drift. It carries
no run or deployment state and takes no parameters. Client-side validation against
this schema is a usability layer only; `POST /api/runs` always re-validates every
submission with `internal/config.Parse` regardless.

### Health endpoints

The worker (and, since sub-issue 2 of the web UI epic, `cmd/web` — see above) serves two
HTTP health endpoints on `HEALTH_ADDR` (default `:8080` for `worker`, `:8081` for `web`)
for Kubernetes probes and the container `HEALTHCHECK`:

| Endpoint | Meaning | Status |
|----------|---------|--------|
| `GET /healthz` | **Liveness** — the process is up and serving. Independent of Temporal connectivity, so a process merely waiting on Temporal to recover is not restarted. | `200` once the server is listening. |
| `GET /readyz` | **Readiness** — the process is usefully connected. Re-checks the Temporal frontend health per request. | `200` when Temporal is reachable and healthy; `503` otherwise. |

Neither endpoint gates or fails a run — they are observational only (SPEC §14). The
endpoints are disabled (no port opened) when `HEALTH_ADDR` is set to an empty value.

The `worker healthcheck` subcommand is a self-probe used as the container `HEALTHCHECK`:
it `GET`s `/readyz` on the local health server and exits `0` when ready, non-zero
otherwise, so `docker inspect` health reflects readiness. It never starts a Temporal
worker. It targets `HEALTH_ADDR` by default; an optional positional `host:port` argument
overrides the target. `cmd/web` has no equivalent self-probe subcommand yet — its
container image can `curl`/equivalent its own `/healthz`/`/readyz` directly.

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
