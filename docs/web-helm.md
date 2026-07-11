# Web UI Helm chart

The **web UI** (`cmd/web`) is a standalone HTTP server: the browser SPA at `/` plus a JSON
API under `/api/*`, gated behind OIDC authentication — see
[`docs/web-ui-design.md`](web-ui-design.md). Unlike the
[control worker](control-worker-helm.md), it is a long-running HTTP service serving live
browser/API traffic, not a queue-driven job — this chart has **no** KEDA `ScaledJob` option,
only a fixed-replica `Deployment`, alongside a `Service` and an optional `Ingress`.

The chart lives at `deploy/charts/tape-archiver-web/`. It is a thin wrapper over the
[bjw-s app-template `common`](https://bjw-s-labs.github.io/helm-charts/docs/) library — the
same base, and the same overall structure, as the
[control-worker chart](control-worker-helm.md) — and by default renders a `Deployment`
running `cmd/web`, plus a `ClusterIP` `Service` exposing its main HTTP port.

## Prerequisites

- A reachable Temporal frontend (`config.temporal.address`).
- A reachable, standards-compliant OIDC identity provider (`config.web.oidc.issuerUrl`) with
  a confidential client registered for this app (`config.web.oidc.clientId` /
  `config.web.oidc.clientSecret`) — see
  [OIDC authentication](configuration.md#oidc-authentication-cmdweb).
- A published web image (`make build-images`, see [web-image.md](web-image.md)). The chart
  defaults the repository to `ghcr.io/soliddowant/tape-archiver/web`; set the tag to a real
  build.
- Two pre-existing Kubernetes `Secret`s (see [Secrets](#secrets-required) below) — this chart
  never accepts the OIDC client secret or the session key as plaintext values.
- `prometheus-operator` CRDs **only** if you enable `config.web.metrics.enabled` (it emits a
  `PodMonitor`).
- An Ingress controller (e.g. `ingress-nginx`) **only** if you enable
  `resources.ingress.main` (see [Ingress](#ingress-optional) below).

## Secrets (required)

Two credentials are **always** sourced from existing Kubernetes `Secret`s via
`secretKeyRef` — never as a plaintext chart value, and never rendered into any manifest this
chart produces:

- **OIDC client secret** (`config.web.oidc.clientSecret.secretKeyRef`) — this app's
  confidential-client secret at the identity provider.
- **Session key** (`config.web.sessionKey.secretKeyRef`) — a base64-encoded 32-byte AES-256
  key (`openssl rand -base64 32`) encrypting the session and login-state cookies. `cmd/web`
  holds no server-side session store, so losing or rotating this key just signs every
  operator out.

Unlike `config.temporal.apiKey` (below), these two have **no** plaintext `value` fallback in
this chart's schema at all — rendering fails with a clear error naming the missing field
rather than silently accepting a value that should never live in a values file or Helm
release history.

```
kubectl create secret generic tape-web-oidc \
  --from-literal=clientSecret='...'
kubectl create secret generic tape-web-session \
  --from-literal=key="$(openssl rand -base64 32)"
```

```yaml
config:
  web:
    oidc:
      clientSecret:
        secretKeyRef:
          name: tape-web-oidc
          key: clientSecret
    sessionKey:
      secretKeyRef:
        name: tape-web-session
        key: key
```

## Install

The chart depends on the bjw-s `common` library chart, fetched at install time:

```
helm dependency update deploy/charts/tape-archiver-web
helm install tape-web deploy/charts/tape-archiver-web \
  --set config.temporal.address=temporal-frontend.temporal.svc.cluster.local:7233 \
  --set config.web.oidc.issuerUrl=https://idp.example.com \
  --set config.web.oidc.clientId=tape-archiver-web \
  --set config.web.oidc.redirectUrl=https://tape-archiver.example.com/auth/callback \
  --set config.web.oidc.clientSecret.secretKeyRef.name=tape-web-oidc \
  --set config.web.oidc.clientSecret.secretKeyRef.key=clientSecret \
  --set config.web.sessionKey.secretKeyRef.name=tape-web-session \
  --set config.web.sessionKey.secretKeyRef.key=key \
  --set resources.controllers.main.containers.main.image.tag=v1.2.3
```

Render and validate without a cluster (the check `make chart-lint` runs):

```
make chart-lint
# or directly:
helm template deploy/charts/tape-archiver-web \
  --set config.temporal.address=temporal-frontend.temporal.svc.cluster.local:7233 \
  --set config.web.oidc.issuerUrl=https://idp.example.com \
  --set config.web.oidc.clientId=tape-archiver-web \
  --set config.web.oidc.redirectUrl=https://tape-archiver.example.com/auth/callback \
  --set config.web.oidc.clientSecret.secretKeyRef.name=tape-web-oidc \
  --set config.web.oidc.clientSecret.secretKeyRef.key=clientSecret \
  --set config.web.sessionKey.secretKeyRef.name=tape-web-session \
  --set config.web.sessionKey.secretKeyRef.key=key
```

`config.temporal.address`, `config.web.oidc.{issuerUrl,clientId,redirectUrl}`, and both
`secretKeyRef` pairs above are all required — rendering fails with a clear message naming
whichever is empty.

## Packaging & release

For a release, the chart is packaged into a versioned `.tgz` rather than installed from the
source tree, exactly like the control-worker chart:

```
make helm                     # packages bin/helm/tape-archiver-{control-worker,web}-$(VERSION).tgz
make helm PUSH_ALL=true       # also pushes both to oci://ghcr.io/soliddowant/charts
```

Both the chart `version` and `appVersion` are stamped from the Makefile's `VERSION` at
package time — the same value that also tags the images (see [web-image.md](web-image.md)).
`make build-all` packages both charts alongside all three images, and `make release` cuts the
git tag and GitHub release for `v$(VERSION)` (a dry run unless `PUSH_ALL=true`).

## Configuration

### Temporal (`config.temporal`)

Identical shape and semantics to the control-worker chart's `config.temporal` — see
[control-worker-helm.md](control-worker-helm.md#temporal-configtemporal) for the full
field-by-field reference (address/namespace/apiKey/tls/grpcMeta). Non-secret fields render
into a `temporal.toml` `ConfigMap` consumed via `TEMPORAL_CONFIG_FILE`; secret material comes
from Kubernetes `Secret`s. There is no KEDA-specific `config.temporal.keda` block in this
chart — it has no `ScaledJob` path.

### Web UI (`config.web`)

| Key | Default | Purpose |
| --- | --- | --- |
| `logLevel` | `info` | `LOG_LEVEL` for the web server. |
| `metricsScrapeWaitTimeout` | `""` | `METRICS_SCRAPE_WAIT_TIMEOUT` override (Go duration, e.g. `60s`). Leave empty for `cmd/web`'s own default of `0s` — no shutdown-time wait for a final scrape, so pods drain within roughly `cmd/web`'s 10-second HTTP shutdown deadline (see `docs/configuration.md`). Setting a positive value delays every pod's SIGTERM exit by up to that long; keep it well under the pod's `terminationGracePeriodSeconds` (chart default 30) or the kubelet will SIGKILL the pod mid-wait. |
| `metrics.enabled` | `false` | Emit a `PodMonitor` for the always-on `/metrics` endpoint (port 9090). Requires prometheus-operator CRDs. |
| `oidc.issuerUrl` | `""` (**required**) | OIDC identity provider issuer URL (used for discovery). |
| `oidc.clientId` | `""` (**required**) | This app's OIDC client ID. |
| `oidc.redirectUrl` | `""` (**required**) | This app's OIDC callback URL, exactly as registered with the provider. |
| `oidc.clientSecret.secretKeyRef.{name,key}` | `""` (**required**) | OIDC client secret from a `Secret` — see [Secrets](#secrets-required). No plaintext fallback. |
| `sessionKey.secretKeyRef.{name,key}` | `""` (**required**) | Session-cookie encryption key from a `Secret` — see [Secrets](#secrets-required). No plaintext fallback. |
| `dryRun.mhvtlChangerDev` / `dryRun.mhvtlDrive0Dev` / `dryRun.mhvtlDrive1Dev` | `""` | mhvtl device nodes a dry-run `POST /api/runs` submission redirects to (`MHVTL_CHANGER_DEV`/`MHVTL_DRIVE0_DEV`/`MHVTL_DRIVE1_DEV`). Leave all three empty (the default, appropriate for a production deployment against a real library) to make dry-run submissions fail closed with `400`. Must be set together or not at all — rendering fails otherwise. |

### Kubernetes resources (`resources`)

`resources` follows the bjw-s app-template schema and is deep-merged **over** the
chart-generated `main` controller and `main` Service, so anything you set wins. The generated
default provides the image, three named container ports — `http` (8080, the SPA + JSON API,
`WEB_LISTEN_ADDRESS`), `health` (8081, `HEALTH_ADDR`), and `metrics` (9090,
`METRICS_ADDR`) — an `httpGet` liveness probe on `/healthz` and an `httpGet` readiness probe
on `/readyz` against the `health` port (readiness is gated on live Temporal connectivity), a
hardened `securityContext` (non-root, read-only rootfs, all capabilities dropped), a 30s
termination grace period, a `ClusterIP` `Service` exposing only the `http` port, and a
`PodDisruptionBudget` when `replicas > 1`.

```yaml
resources:
  controllers:
    main:
      replicas: 2
      containers:
        main:
          image:
            repository: ghcr.io/soliddowant/tape-archiver/web
            tag: v1.2.3
          resources:
            requests: { cpu: 100m, memory: 128Mi }
```

> The web UI reads no Kubernetes API and needs no ServiceAccount token
> (`automountServiceAccountToken` stays at its bjw-s default of `false`), unlike the control
> worker, which needs the Kubernetes API for snapshot discovery.

### Ingress (optional)

`resources.ingress` is entirely operator-configured — the chart computes nothing beyond the
`main` Service above, which a single-host/single-path Ingress auto-detects as its backend
(bjw-s' ingress-to-Service auto-detection: it applies whenever the chart renders exactly one
enabled Service, which this chart always does) when a path's `service` is left unset.
Disabled by default (no entries).

```yaml
resources:
  ingress:
    main:
      enabled: true
      className: nginx
      annotations:
        cert-manager.io/cluster-issuer: letsencrypt-prod
      hosts:
        - host: tape-archiver.example.com
          paths:
            - path: /
      tls:
        - hosts:
            - tape-archiver.example.com
          secretName: tape-archiver-web-tls
```

Whatever hostname is used here must exactly match `config.web.oidc.redirectUrl`'s origin (and
be registered as the OIDC client's redirect URI at the identity provider) — the web UI's
`/auth/callback` route only ever completes a login that started from that exact configured
redirect URL.

## Example: full production install

```yaml
config:
  temporal:
    address: temporal-frontend.temporal.svc.cluster.local:7233
  web:
    oidc:
      issuerUrl: https://idp.example.com
      clientId: tape-archiver-web
      redirectUrl: https://tape-archiver.example.com/auth/callback
      clientSecret:
        secretKeyRef:
          name: tape-web-oidc
          key: clientSecret
    sessionKey:
      secretKeyRef:
        name: tape-web-session
        key: key
    metrics:
      enabled: true
resources:
  controllers:
    main:
      containers:
        main:
          image:
            tag: v1.2.3
  ingress:
    main:
      enabled: true
      className: nginx
      annotations:
        cert-manager.io/cluster-issuer: letsencrypt-prod
      hosts:
        - host: tape-archiver.example.com
          paths:
            - path: /
      tls:
        - hosts:
            - tape-archiver.example.com
          secretName: tape-archiver-web-tls
```

## Verifying the deployment

Once the pod is `Running` and `Ready` (readiness reflects live Temporal connectivity — see
[Health endpoints](configuration.md#health-endpoints)), the web UI is reachable through the
`Service` (or the `Ingress`, once DNS/TLS are in place) and immediately redirects an
unauthenticated browser to `/auth/login`. A pod that stays up rather than crash-looping
confirms `cmd/web` connected to Temporal and completed OIDC discovery against
`config.web.oidc.issuerUrl` at startup.
