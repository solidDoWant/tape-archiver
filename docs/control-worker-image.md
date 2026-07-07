# Control-worker OCI image

The **control worker** runs in Kubernetes and orchestrates runs on the `control` task
queue: it drives the backup workflow, discovers ZFS snapshots via the Kubernetes API, and
posts failure notifications to Discord (`DISCORD_FAILURE_WEBHOOK_URL`). It never touches
bulk data or tape hardware — that is the data worker's job (SPEC §4.1: "Lightweight, no
bulk data"). It is the same `cmd/worker` binary as the data worker; the `ROLE` env var
selects the `control` task queue at run time.

The image is built reproducibly with Nix (`pkgs.dockerTools.streamLayeredImage`, per
media-processor) from the flake's single pinned `nixpkgs`. It bundles the **same shared
`packages.worker` base** as the data-worker image (`nix/data-worker-image.nix`, #73), so
both images run the byte-for-byte identical worker binary from the same pinned source.

## Contents

The image is deliberately minimal:

| Component | Source | Purpose |
| --- | --- | --- |
| `worker` | `nix/worker.nix` (`packages.worker`) | the Temporal worker binary; entrypoint `/bin/worker` |
| CA certificates | `cacert` | TLS roots for the Temporal control plane and the Discord webhook |

It carries **none** of the bulk-data/tape tooling the data-worker image bundles — no
`ltfs`/`mkltfs`, `mt`, `par2`, `age`, `zstd`, `sg3_utils`, or `lsscsi`. The
control worker shells out to no such tools, so shipping them would only add weight and
attack surface.

The image labels record the pinned `nixpkgs` revision
(`docker inspect --format '{{json .Config.Labels}}'`); the source tag is the short
`nixpkgs` revision (a deterministic, content-anchored tag — never a floating `latest`).

## Building

```
make build-images
```

This builds both worker images (`.#dataWorkerImage` and `.#controlWorkerImage`), streams
each into the local Docker daemon, and tags them. The control image name is
`tape-archiver-control-worker`.

Registry name, version, and publishing are controlled by variables (media-processor
pattern), shared with the data-worker flow:

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

The control image is pushed to
`$CONTAINER_REGISTRY/tape-archiver/control-worker:$VERSION` (and `:latest` when
`PUSH_ALL=true`). Registry credentials are assumed to already be present in the Docker
daemon (there is no `docker login` step). Automated/CI push and multi-architecture builds
are out of scope. This is the published image the control-worker Helm chart (#74)
references.

## Runtime requirements

Unlike the data worker, the control worker is lightweight and needs no host access: **no**
device passthrough, **no** `/dev/fuse`, and **no** bind mounts. It reaches the Temporal
control plane and the Discord webhook over the network, using the bundled TLS roots.

The entrypoint is `/bin/worker`; run configuration is supplied via the environment
(including `DISCORD_FAILURE_WEBHOOK_URL`, SPEC §4.1, §11) and the Temporal workflow
payload (SPEC §4.1, §5).

Optionally set `WORKER_IDLE_EXIT_AFTER` (a Go duration, e.g. `15m`) so the control worker
drains and exits `0` after an idle window, letting a KEDA-spawned `Job` scale back to
zero; it is disabled by default. See
[Control-worker idle-exit](configuration.md#control-worker-idle-exit) for the behavior and
the exported gauges.

## Health check

The image declares a `HEALTHCHECK` (`docker inspect --format '{{json .Config.Healthcheck}}'`)
that runs the worker's own `healthcheck` self-probe — the image ships no `curl`/`wget`, so
the binary probes its local `/readyz` endpoint and exits `0`/non-zero. Container health thus
reflects **readiness**: a worker that has lost its Temporal connection reports unhealthy. The
probe targets `HEALTH_ADDR` (default `:8080`); see [`docs/configuration.md`](configuration.md)
for the endpoints.
