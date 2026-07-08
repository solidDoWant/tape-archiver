# Data-worker OCI image

The **data worker** runs as a container on the storage host (`ubuntu-storage-host-01`)
and performs every bulk-data activity where the bytes already live — `tar`, `age`, PAR2,
checksums, LTFS format/mount/write, and library moves — plus building the PDF report (and
the recovery ISO when optical burning is enabled) and delivering the report to Discord
(SPEC §4.1, §4.3). It is the same `cmd/worker` binary as the control worker; the `ROLE`
env var selects the `data` task queue at run time.

The image is built reproducibly with Nix (`pkgs.dockerTools.streamLayeredImage`, per
media-processor) from the flake's single pinned `nixpkgs`. Every external tool it bundles
therefore comes from the **same source of truth** as the static recovery-binary set that
ships on the recovery disc (`nix/recovery-binaries.nix`, SPEC §10) — backup tooling and
recovery tooling can never drift. This equality is asserted at build time
(`nix/data-worker-image.nix`), so a `nixpkgs` bump that changed a version would fail the
build rather than silently diverge from the disc.

The image also **bakes the static recovery set itself in** at `/recovery/{bin,src}` (the
`recoveryBinaries` derivation's `bin/` binaries and `src/` source archives) and defaults
`TAPE_RECOVERY_BINARIES_DIR`/`TAPE_RECOVERY_SOURCES_DIR` there. The Report phase stages
these onto the recovery ISO, so shipping them inside the image — rather than via a mounted,
operator-populated directory — makes the disc bytes the same store paths the image runs:
the drift guarantee above becomes structural, and the data worker needs no recovery-set
bind mount or `make recovery-binaries` populate step. Override the two variables only to
relocate the set.

## Bundled tooling and pinned versions

Versions are pinned by the flake's `nixpkgs` revision and recorded as image labels
(`docker inspect --format '{{json .Config.Labels}}'`). At the current pin:

| Tool | Package | Version | Purpose |
| --- | --- | --- | --- |
| `mkltfs` / `ltfs` / `ltfsck` | `nix/ltfs.nix` (reference LTFS) | 2.4.8.4 | LTFS format, mount, write (SPEC §6) |
| `age` | `age` | 1.3.1 | encryption, native post-quantum recipients (SPEC §7) |
| `par2` | `par2cmdline-turbo` | 1.4.0 | per-archive PAR2 recovery sets (SPEC §8) |
| `zstd` | `zstd` | 1.5.7 | optional pre-encryption compression (SPEC §4.3) |
| `tar` | `gnutar` | 1.35 | recovery-disc tool parity (archives are written by the Go `archive/tar` writer, not this binary) |
| `xorriso` | `xorriso` | 1.5.6 | burn/verify the optical recovery disc (`pkg/optical`, SPEC §10) |
| `mt` | `mt-st` | — | drive control (non-rewinding `nst` nodes) |
| `sg_logs` / `sg_turs` | `sg3_utils` | — | TapeAlert / log pages, drive readiness (SPEC §3) |
| `lsscsi` | `lsscsi` | — | enumerate SCSI tape/changer devices |
| `zfs` | `zfs` | — | read dataset properties + locate `.zfs/snapshot/` (needs `/dev/zfs`) |

`age`, `par2`, `zstd`, and `tar` are the tools the recovery disc also stages; their
versions are identical to the disc by construction (build-time assert). `xorriso` is the
one exception among the bulk-data tools: it burns and verifies the recovery disc but is
**not** staged onto it — recovery only requires *reading* ISO 9660, which any DVD drive
mounts, so shipping a burn tool on the disc would be dead weight (SPEC §10).

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
| `VERSION` | `0.0.1-dev` | the registry image tag (a single, manually-bumped value; the one place a release is versioned) |
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
cannot bake in (SPEC §4.1). A committed **reference systemd unit** encodes all of the
following by construction — see [`deploy/data-worker/`](../deploy/data-worker) (the unit,
an example environment file, and install/validation notes). It is a correct-by-default
starting point for the storage host (`ubuntu-storage-host-01`), still managed out-of-band;
`make unit-lint` validates it without a running service. When the unit runs the image, it
must provide:

- **Device passthrough** for the drives and changer: `/dev/nst0`, `/dev/nst1`,
  `/dev/sch0`, and the drives' `/dev/sg*` nodes.
- **`/dev/fuse` + `CAP_SYS_ADMIN`** — LTFS is FUSE-based and cannot mount without them.
- **Bind mount of `/mnt/bulk-pool-01`** with `shared`/`rslave` mount propagation, so the
  ZFS snapshot views (`.zfs/snapshot/<snap>/`) and the staging directory are visible
  inside the container. The image pre-creates the generic `/mnt` mount root; Docker
  auto-creates the actual bind-mount target (e.g. `/mnt/bulk-pool-01`) under it at run
  time, so no site-specific path is baked into the image.
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
