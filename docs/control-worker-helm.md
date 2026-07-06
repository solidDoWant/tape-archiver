# Control-worker Helm chart

The **control worker** runs in Kubernetes and orchestrates backup runs on the `control`
task queue: it drives the workflow, discovers ZFS snapshots via the Kubernetes API, and
posts run-failure notifications to Discord. It never touches bulk data or tape hardware —
that is the data worker's job (SPEC §4.1: "Lightweight, no bulk data"). The data worker is
run by systemd-managed Docker on the storage host and has **no** Helm chart; this chart
deploys the control worker only.

The chart lives at `deploy/charts/tape-archiver-control-worker/`. It is a thin wrapper over
the [bjw-s app-template `common`](https://bjw-s-labs.github.io/helm-charts/docs/) library
(the same base as [media-processor](https://github.com/solidDoWant/media-processor)) and by
default renders a single `Deployment` running the shared `cmd/worker` binary with
`ROLE=control`. It can optionally render a KEDA `ScaledJob` that scales the worker to zero
between runs instead — see [Autoscaling](#autoscaling-keda-scale-to-zero) below.

## Prerequisites

- A reachable Temporal frontend (`config.temporal.address`).
- A published control-worker image (`make build-images`, see
  [control-worker-image.md](control-worker-image.md)). The chart defaults the repository to
  `ghcr.io/soliddowant/tape-archiver/control-worker`; set the tag to a real build.
- `prometheus-operator` CRDs **only** if you enable `config.controlWorker.metrics.enabled`
  (it emits a `PodMonitor`).

## Install

The chart depends on the bjw-s `common` library chart, fetched at install time:

```
helm dependency update deploy/charts/tape-archiver-control-worker
helm install tape-control deploy/charts/tape-archiver-control-worker \
  --set config.temporal.address=temporal-frontend.temporal.svc.cluster.local:7233 \
  --set resources.controllers.main.containers.main.image.tag=v1.2.3
```

Render and validate without a cluster (the check `make chart-lint` runs):

```
make chart-lint
# or directly:
helm template deploy/charts/tape-archiver-control-worker \
  --set config.temporal.address=temporal-frontend.temporal.svc.cluster.local:7233
```

`config.temporal.address` is required — rendering fails with a clear message when it is
empty.

## Configuration

### Temporal (`config.temporal`)

Non-secret connection settings render into a `temporal.toml` `ConfigMap` consumed by the
Temporal Go SDK `envconfig` loader (via `TEMPORAL_CONFIG_FILE`); secret material comes from
Kubernetes `Secret`s. The task queue is **not** configurable — the worker derives `control`
from `ROLE`, which the chart sets unconditionally.

| Key | Default | Purpose |
| --- | --- | --- |
| `address` | `""` (**required**) | Temporal frontend `host:port` (no scheme). |
| `namespace` | `default` | Temporal namespace. |
| `apiKey.value` | `""` | API key, inline (dev only). Mutually exclusive with `secretKeyRef`. A `file:///abs/path` value is read on every RPC so external rotators are picked up without a restart. |
| `apiKey.secretKeyRef.{name,key}` | `""` | API key from a `Secret`. Both fields required together. Surfaced as `TEMPORAL_API_KEY`. |
| `tls.enabled` | `false` | Server-side TLS. `false` = plaintext gRPC even when an API key is set. |
| `tls.serverName` | `""` | SNI / cert-verification hostname override. |
| `tls.disableHostVerification` | `false` | Skip server-cert hostname verification (dev/self-signed only). |
| `tls.clientCertificate.secretName` | `""` | mTLS: a `kubernetes.io/tls` `Secret` (`tls.crt`/`tls.key`), mounted read-only. |
| `tls.caCert.secretKeyRef.{name,key}` | key `ca.crt` | Custom CA bundle from a `Secret`, mounted read-only. |
| `grpcMeta` | `{}` | gRPC metadata headers added to every RPC. |

Setting any `tls.*` cert reference while `tls.enabled` is `false` is a render-time error,
so a forgotten `enabled` flag never silently disables mTLS or CA verification.

### Control worker (`config.controlWorker`)

| Key | Default | Purpose |
| --- | --- | --- |
| `logLevel` | `info` | `LOG_LEVEL` for the worker. |
| `discordFailureWebhookUrl.value` | `""` | Discord failure-alert webhook, inline (dev only). Mutually exclusive with `secretKeyRef`. |
| `discordFailureWebhookUrl.secretKeyRef.{name,key}` | `""` | Webhook URL from a `Secret`. Surfaced as `DISCORD_FAILURE_WEBHOOK_URL` (SPEC §11). Leave both empty to disable failure alerting. |
| `k8sDatasetParent` | `""` | democratic-csi `datasetParentName`, prepended to a relative CSI `snapshotHandle` during k8s resolution (SPEC §3). Surfaced as `TAPE_K8S_DATASET_PARENT`. Only needed when a run names k8s sources. |
| `metrics.enabled` | `false` | Emit a `PodMonitor` for the always-on `/metrics` endpoint (port 9090). Requires prometheus-operator CRDs. |

### Kubernetes resources (`resources`)

`resources` follows the bjw-s app-template schema and is deep-merged **over** the
chart-generated `main` controller, so anything you set wins. The generated default provides
the image, a named `health` port (8080) and `metrics` port (9090), an `httpGet` liveness
probe on `/healthz` and an `httpGet` readiness probe on `/readyz` against the health port
(readiness is gated on live Temporal connectivity), a hardened `securityContext` (non-root,
read-only rootfs, all capabilities dropped), a 60s termination grace period, and a
`PodDisruptionBudget` when `replicas > 1`.

```yaml
resources:
  controllers:
    main:
      replicas: 2
      containers:
        main:
          image:
            repository: ghcr.io/soliddowant/tape-archiver/control-worker
            tag: v1.2.3
          resources:
            requests: { cpu: 100m, memory: 128Mi }
```

> Snapshot discovery reads `VolumeSnapshot` resources from the Kubernetes API via an
> in-cluster client, so the chart mounts the pod's ServiceAccount token
> (`automountServiceAccountToken: true`). Granting that ServiceAccount the necessary RBAC —
> binding a `Role`/`ClusterRole` for `VolumeSnapshot`s to the chart-created ServiceAccount —
> is left to the operator and is outside this chart's scope.

## Autoscaling (KEDA scale-to-zero)

A backup run fires only once every few months, yet the default `Deployment` keeps a
control-worker pod polling the `control` queue 24/7. Setting
`resources.controllers.main.type: scaledjob` opts into a [KEDA](https://keda.sh/)
`ScaledJob` instead: **nothing runs in the cluster between runs**, and a run submitted to
the `control` queue scales the worker `0 → 1` on demand. The worker then drains and exits
after an idle window (`WORKER_IDLE_EXIT_AFTER`, defaulted to `15m` on this path) so its
`Job` completes and the deployment scales back to zero. Because the tape-write window is
*hours* and operator pauses can be *days* — both of which leave the `control` queue idle —
the worker will exit and respawn mid-run; on respawn it replays workflow history and
continues. A resume signal (`tapectl resume`) lands on the Temporal server regardless of
worker presence and re-triggers KEDA within the polling interval.

Autoscaling is **strictly opt-in**: with `type` absent (the default) the render is
byte-identical to the fixed-replica `Deployment`. The **data worker is unaffected** — it is
systemd-managed Docker on the storage host, outside Kubernetes and KEDA's reach.

### Cluster prerequisites

- **KEDA `>= 2.17`** installed in the cluster — the Temporal scaler is bundled with KEDA
  from 2.17 onward. The chart renders `keda.sh/v1alpha1` `ScaledJob` (and a
  `TriggerAuthentication` when a KEDA credential is configured) resources; installing and
  managing the KEDA operator and its CRDs is the operator's responsibility, not this chart's.
- **Temporal server `>= 1.24`** — the scaler reads `ApproximateBacklogCount` from
  `DescribeTaskQueue`, which older servers leave at `0`, so KEDA would never scale up.

### KEDA credential (`config.temporal.keda`)

KEDA authenticates to Temporal with a **separate, least-privilege** credential — read-only
`DescribeTaskQueue` — distinct from the worker's own `config.temporal.apiKey` / `tls.*`, so
the scaler is never handed the worker's key. When set, it is wired through a per-release
`TriggerAuthentication`.

The credential is **optional**: an authenticated Temporal frontend needs
`config.temporal.keda.apiKey` (and any `keda.tls.*`), but an **unauthenticated, plaintext**
frontend needs neither — leave `config.temporal.keda` empty and the scaler connects
anonymously (no `TriggerAuthentication` is emitted). Note that KEDA's Temporal scaler forces
a TLS connection whenever an API key is set, so a plaintext frontend must be configured with
**no** `keda.apiKey`. This block is unused on the default `Deployment` path.

| Key | Default | Purpose |
| --- | --- | --- |
| `apiKey.value` | `""` | KEDA API key, inline (dev only). Chart materialises a `Secret` (`<release>-keda-temporal-apikey`). Mutually exclusive with `secretKeyRef`. |
| `apiKey.secretKeyRef.{name,key}` | `""` | KEDA API key from a `Secret`. Both fields required together. |
| `tls.enabled` | `false` | Server-side TLS for KEDA's connection (independent of `config.temporal.tls`). |
| `tls.serverName` | `""` | SNI / cert-verification hostname override (KEDA `tlsServerName`). |
| `tls.disableHostVerification` | `false` | Skip server-cert hostname verification (KEDA `unsafeSsl`; dev/self-signed only). |
| `tls.clientCertificate.secretName` | `""` | mTLS: a `kubernetes.io/tls` `Secret` (`tls.crt`/`tls.key`), referenced as KEDA authParams. |
| `tls.caCert.secretKeyRef.{name,key}` | `""` | Custom CA bundle from a `Secret` (both fields required together). |

Setting any `keda.tls.*` reference while `keda.tls.enabled` is `false` is a render-time
error, mirroring the worker's `tls.*` guards.

### Tuning the ScaledJob

KEDA-level knobs live under `resources.controllers.main.keda.*` and Job-template knobs under
`resources.controllers.main.job.*`:

| Key | Default | Purpose |
| --- | --- | --- |
| `keda.maxReplicaCount` | `1` | Max concurrent worker Jobs — the control worker is a singleton, so `0 → 1`. |
| `keda.pollingInterval` | KEDA default (`30`) | Seconds between backlog polls. |
| `keda.targetQueueSize` | `5` | Backlog per Job before scaling out. |
| `keda.activationTargetQueueSize` | `0` | Backlog that activates `0 → 1` (any queued task). |
| `keda.successfulJobsHistoryLimit` / `keda.failedJobsHistoryLimit` / `keda.scalingStrategy` | KEDA defaults | Passed through when set. |
| `job.backoffLimit` / `job.ttlSecondsAfterFinished` / `job.parallelism` / `job.completions` / `job.activeDeadlineSeconds` | Kubernetes defaults | `jobTargetRef.spec` fields, passed through when set. |
| `containers.main.env.WORKER_IDLE_EXIT_AFTER` | `15m` | Idle window before the worker drains and exits; overrides the chart default. |

```yaml
config:
  temporal:
    address: temporal-frontend.temporal.svc.cluster.local:7233
    keda:
      apiKey:
        secretKeyRef:
          name: keda-temporal-key
          key: key
resources:
  controllers:
    main:
      type: scaledjob
      keda:
        pollingInterval: 30
      job:
        ttlSecondsAfterFinished: 600
```

## Example: authenticated Temporal Cloud + secret webhook

```yaml
config:
  temporal:
    address: my-ns.abc12.tmprl.cloud:7233
    namespace: my-ns.abc12
    tls:
      enabled: true
    apiKey:
      secretKeyRef:
        name: temporal-api-key
        key: key
  controlWorker:
    discordFailureWebhookUrl:
      secretKeyRef:
        name: discord-webhook
        key: url
```

## Verifying the worker registered

Once the pod is `Running`, it appears in the Temporal UI as a poller on the `control` task
queue. The worker exits non-zero if it cannot dial or health-check the Temporal frontend at
startup, so a pod that stays up (rather than crash-looping) confirms a live connection.
