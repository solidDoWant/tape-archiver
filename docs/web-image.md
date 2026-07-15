# Web UI OCI image

The **web UI** (`cmd/web`) is a standalone HTTP server that serves the browser SPA at `/`
and a read-only-plus-submit JSON API under `/api/*` (`pkg/runsapi`, `pkg/runsubmit`),
gated behind OIDC authentication (`pkg/webauth`) — see
[`docs/web-ui-design.md`](web-ui-design.md). It is its own process and its own image,
separate from the `worker` binary the [control-worker](control-worker-image.md) and data
worker use: it talks only to the Temporal frontend and the OIDC identity provider, and
never touches tape hardware or bulk data.

The image is built reproducibly with Nix (`pkgs.dockerTools.streamLayeredImage`, the same
approach as the [control-worker image](control-worker-image.md)) from the flake's single
pinned `nixpkgs`. Unlike the worker images, building it also requires building the `web/`
frontend; that happens hermetically too — `nix/web-frontend.nix` runs `npm ci`-equivalent
dependency fetch (pinned by a committed `npmDepsHash`) and `npm run build` entirely inside
the Nix sandbox, with no network access at build time, and `nix/web.nix` embeds that build
output into the `cmd/web` binary via `go:embed` before compiling it. This mirrors, but does
not replace, `make frontend-build` (a plain `npm run build` on the host) — both produce the
same `web/` sources built the same way; `make build`/`bin/web` uses the host `npm` for a
fast local dev loop, while `nix build .#web`/`.#webImage` reproduce the exact same build
hermetically for the shipped image.

## Contents

The image is deliberately minimal, the same posture as the control-worker image:

| Component | Source | Purpose |
| --- | --- | --- |
| `web` | `nix/web.nix` (`packages.web`) | the web UI HTTP server binary (embeds the built SPA); entrypoint `/bin/web` |
| CA certificates | `cacert` | TLS roots for the Temporal frontend and the OIDC identity provider |
| `curl` (`curlMinimal`) | nixpkgs | used **only** by the container `HEALTHCHECK` — see [Health check](#health-check) below |

It carries **none** of the bulk-data/tape tooling the data-worker image bundles, and none of
the `ltfs`/`mt`/`par2`/`age`/`zstd`/`sg3_utils`/`lsscsi` either worker image needs — the web
UI shells out to none of them.

The image labels record the pinned `nixpkgs` revision
(`docker inspect --format '{{json .Config.Labels}}'`); the source tag is the short `nixpkgs`
revision (a deterministic, content-anchored tag — never a floating `latest`).

## Building

```
make build-images
```

This builds all three images (`.#dataWorkerImage`, `.#controlWorkerImage`, and
`.#webImage`), streams each into the local Docker daemon, and tags them. The web image name
is `tape-archiver-web`.

Registry name, version, and publishing are controlled by the same variables as the worker
images (media-processor pattern):

| Variable | Default | Effect |
| --- | --- | --- |
| `CONTAINER_REGISTRY` | `ghcr.io/soliddowant` | registry/namespace prefix for the tag(s) |
| `VERSION` | `0.0.1-dev` | the registry image tag (a single, manually-bumped value; the one place a release is versioned) |
| `PUSH_ALL` | `false` | when `true`, also tag `:latest` and `docker push` every tag |

The default (`PUSH_ALL=false`) only builds and loads locally — it never publishes.
Publishing is a manual, opt-in operator step:

```
make build-images VERSION=v1.2.3 PUSH_ALL=true
```

The web image is pushed to `$CONTAINER_REGISTRY/tape-archiver/web:$VERSION` (and `:latest`
when `PUSH_ALL=true`). Registry credentials are assumed to already be present in the Docker
daemon (there is no `docker login` step). This is the published image the
[web Helm chart](web-helm.md) references.

## Runtime requirements

Like the control worker, the web UI is lightweight and needs no host access: **no** device
passthrough, **no** `/dev/fuse`, and **no** bind mounts. It reaches the Temporal frontend and
the OIDC identity provider over the network, using the bundled TLS roots.

The entrypoint is `/bin/web`. Configuration is entirely environment-driven — see
[Web UI environment variables](configuration.md#web-ui-environment-variables-cmdweb) for the
full list (`TEMPORAL_ADDRESS`, `OIDC_*`, `WEB_SESSION_KEY`, `WEB_LISTEN_ADDRESS`, and so on).
`cmd/web` refuses to start if it cannot reach Temporal or if the OIDC configuration is
incomplete or malformed, the same fail-fast posture `worker`/`tapectl` use for Temporal
connectivity.

## Health check

The image declares a `HEALTHCHECK` (`docker inspect --format
'{{json .Config.Healthcheck}}'`) that curls its own `/healthz` directly
(`http://127.0.0.1:8081/healthz`, matching `HEALTH_ADDR`'s default). This differs from the
worker images' `HEALTHCHECK`, which instead runs the `worker healthcheck` self-probe
subcommand and ships no HTTP client at all: `cmd/web` has no equivalent self-probe
subcommand (adding one was out of scope when this image was introduced —
packaging only, no `cmd/web` application-code changes), so this image instead bundles a
minimal `curl` build (`curlMinimal`) for the sole purpose of this `HEALTHCHECK`. Container
health here reflects **liveness**, not readiness (unlike the worker images' probe, which
happens to probe `/readyz`) — see [Health endpoints](configuration.md#health-endpoints) for
the distinction. Readiness (Temporal connectivity) is instead what the Helm chart's
Kubernetes `readinessProbe` checks, which is what actually gates traffic in a real
deployment; the container-level `HEALTHCHECK` only needs to answer "is the process still
serving requests at all."

If a deployment overrides `HEALTH_ADDR` away from its `:8081` default, the container
`HEALTHCHECK` (which hardcodes port `8081`) no longer matches and should be overridden too —
the [web Helm chart](web-helm.md) always sets `HEALTH_ADDR` to match its own hardcoded health
container port, so this only matters for a bespoke, non-Helm deployment of this image.
