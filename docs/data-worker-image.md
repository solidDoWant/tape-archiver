# Data-worker OCI image

The **data worker** runs as a container on the storage host (`ubuntu-storage-host-01`)
and performs every bulk-data activity where the bytes already live — `tar`, `age`, PAR2,
checksums, LTFS format/mount/write, and library moves — plus building the PDF report and
recovery ISO and delivering them to Discord (SPEC §4.1, §4.3). It is the same
`cmd/worker` binary as the control worker; the `role` env var selects the `data` task
queue at run time.

The image is built reproducibly with Nix (`pkgs.dockerTools.streamLayeredImage`, per
media-processor) from the flake's single pinned `nixpkgs`. Every external tool it bundles
therefore comes from the **same source of truth** as the static recovery-binary set that
ships on the recovery disc (`nix/recovery-binaries.nix`, SPEC §10) — backup tooling and
recovery tooling can never drift. This equality is asserted at build time
(`nix/data-worker-image.nix`), so a `nixpkgs` bump that changed a version would fail the
build rather than silently diverge from the disc.

## Bundled tooling and pinned versions

Versions are pinned by the flake's `nixpkgs` revision and recorded as image labels
(`docker inspect --format '{{json .Config.Labels}}'`). At the current pin:

| Tool | Package | Version | Purpose |
| --- | --- | --- | --- |
| `mkltfs` / `ltfs` / `ltfsck` | `nix/ltfs.nix` (reference LTFS) | 2.4.8.4 | LTFS format, mount, write (SPEC §6) |
| `age` | `age` | 1.3.1 | encryption, native post-quantum recipients (SPEC §7) |
| `par2` | `par2cmdline-turbo` | 1.4.0 | per-archive PAR2 recovery sets (SPEC §8) |
| `zstd` | `zstd` | 1.5.7 | optional pre-encryption compression (SPEC §4.3) |
| `tar` | `gnutar` | 1.35 | archive creation (SPEC §4.3) |
| `mt` | `mt-st` | — | drive control (non-rewinding `nst` nodes) |
| `mtx` | `mtx` | — | media-changer moves (SPEC §3) |
| `sg_logs` / `sg_turs` | `sg3_utils` | — | TapeAlert / log pages, drive readiness (SPEC §3) |
| `lsscsi` | `lsscsi` | — | enumerate SCSI tape/changer devices |
| `zfs` | `zfs` | — | read dataset properties + locate `.zfs/snapshot/` (needs `/dev/zfs`) |

`age`, `par2`, `zstd`, and `tar` are the tools the recovery disc also stages; their
versions are identical to the disc by construction (build-time assert).

## Building

```
make build-images
```

This builds `.#dataWorkerImage`, streams it into the local Docker daemon, and tags it.
The image name is `tape-archiver-data-worker` and the source tag is the short `nixpkgs`
revision (a deterministic, content-anchored tag — never a floating `latest`).

Registry name, version, and publishing are controlled by variables (media-processor
pattern):

| Variable | Default | Effect |
| --- | --- | --- |
| `CONTAINER_REGISTRY` | `ghcr.io/soliddowant` | registry/namespace prefix for the tag(s) |
| `VERSION` | `git describe --tags --always --dirty` | the registry image tag |
| `PUSH_ALL` | `false` | when `true`, also tag `:latest` and `docker push` every tag |

The default (`PUSH_ALL=false`) only builds and loads locally — it never publishes.
Publishing is a manual, opt-in operator step:

```
make build-images VERSION=v1.2.3 PUSH_ALL=true
```

Registry credentials are assumed to already be present in the Docker daemon (there is no
`docker login` step). Automated/CI push and multi-architecture builds are out of scope.

## Runtime requirements

Tape and LTFS are physical and privileged, so the container needs host access the image
cannot bake in (SPEC §4.1). When the storage-host systemd unit (managed out-of-band)
runs the image, it must provide:

- **Device passthrough** for the drives and changer: `/dev/nst0`, `/dev/nst1`,
  `/dev/sch0`, and the drives' `/dev/sg*` nodes.
- **`/dev/fuse` + `CAP_SYS_ADMIN`** — LTFS is FUSE-based and cannot mount without them.
- **Bind mount of `/mnt/bulk-pool-01`** with `shared`/`rslave` mount propagation, so the
  ZFS snapshot views (`.zfs/snapshot/<snap>/`) and the staging directory are visible
  inside the container. The image pre-creates the `/mnt/bulk-pool-01` mount point.
- **Staging** happens in a plain subdirectory of an existing dataset
  (`bulk-pool-01/archive/.tape-staging/`); the worker never runs `zfs create`.
- **`/dev/zfs` passthrough.** The data-side Resolve/Verify/Prepare activities read ZFS
  dataset properties (`logicalreferenced`, `mountpoint`, user properties) and locate the
  `.zfs/snapshot/` tree through the `zfs` CLI (`pkg/zfs`). `zfs get` issues read-only
  ioctls against `/dev/zfs`, so the device must be passed through even though the worker
  never mutates the pool. The pool bind mount must use `shared`/`rslave` propagation so
  the on-demand `.zfs/snapshot/<snap>/` automounts are visible inside the container.

The entrypoint is `/bin/worker`; run configuration is supplied via the environment and
the Temporal workflow payload (SPEC §4.1, §5).

## Health check

The image declares a `HEALTHCHECK` (`docker inspect --format '{{json .Config.Healthcheck}}'`)
that runs the worker's own `healthcheck` self-probe — the image ships no `curl`/`wget`, so
the binary probes its local `/readyz` endpoint and exits `0`/non-zero. Container health thus
reflects **readiness**: a worker that has lost its Temporal connection reports unhealthy. The
probe targets `HEALTH_ADDR` (default `:8080`); see [`docs/configuration.md`](configuration.md)
for the endpoints.
