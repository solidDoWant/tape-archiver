# Data-worker systemd unit (reference)

A reference systemd unit that runs the data-worker OCI image
([`docs/data-worker-image.md`](../../docs/data-worker-image.md)) as a Docker container on
the storage host, with systemd as the sole supervisor. It is the data-worker counterpart
to the control worker's Helm chart under [`deploy/charts/`](../charts): executable
documentation for the runtime requirements in SPEC §4.1, kept in tree so it cannot drift
from prose.

This is a **correct-by-default starting point for the single known storage host
(`ubuntu-storage-host-01`)**, not blessed production config. The host's copy is managed
out-of-band; treat this as the template.

## Contents

| File | Purpose |
| --- | --- |
| `tape-archiver-data-worker.service` | The systemd unit (`docker run`, foreground, `Type=simple`). |
| `data-worker.env.example` | The run-configuration contract, sourced by both `EnvironmentFile` and `--env-file`. |

## What the unit encodes (SPEC §4.1)

- **Device passthrough:** `/dev/nst0`, `/dev/nst1`, `/dev/sch0`, the drives' `/dev/sg*`
  nodes, `/dev/zfs`, and `/dev/fuse`.
- **Capabilities:** `SYS_ADMIN` (LTFS is FUSE-based) and `SYS_RAWIO` (raw SCSI via SG_IO —
  the changer, `sg_raw`, and LTFS need it).
- **Pool bind mount** of `/mnt/bulk-pool-01` with `rslave` propagation, plus an
  `ExecStartPre` that makes the host subtree rshared — so `.zfs/snapshot/<snap>/`
  automounts are visible inside the container. Getting this wrong loses snapshot data
  silently, so the unit sets it by construction.
- **Content-anchored image tag** and `--pull=never`: the unit never pulls on start.
- **Graceful stop** with a generous `docker stop --time`, so an in-flight activity drains
  rather than being killed mid-write.
- **No hardcoded config:** everything comes from the env file.

## Install

1. Load the pinned image into the host's Docker daemon (from `make build-images`) and note
   its content-anchored tag (short nixpkgs revision or release version).
2. Edit `tape-archiver-data-worker.service` and set `TAPE_ARCHIVER_IMAGE` to that exact
   tag. Confirm the drives' `/dev/sg*` node numbers on this host with `lsscsi -g` and
   adjust the `--device /dev/sg*` lines to match.
3. Install the config:
   ```sh
   sudo install -D -m640 data-worker.env.example /etc/tape-archiver/data-worker.env
   sudoedit /etc/tape-archiver/data-worker.env   # fill in Temporal + paths
   ```
4. Install and enable the unit:
   ```sh
   sudo install -D -m644 tape-archiver-data-worker.service \
     /etc/systemd/system/tape-archiver-data-worker.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now tape-archiver-data-worker.service
   ```
5. Check it is polling the data queue: `journalctl -u tape-archiver-data-worker -f`.

## Validate without a host

```sh
make unit-lint
```

runs `systemd-analyze verify` against the unit — no running service or tape hardware
required, mirroring `make chart-lint`.
