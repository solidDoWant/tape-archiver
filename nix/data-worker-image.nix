# Reproducible OCI image for the data worker (SPEC §4.1).
#
# The data worker runs as a container on the storage host and performs every
# bulk-data activity where the bytes already live: tar, age, PAR2, checksums,
# LTFS format/mount/write, and library moves (SPEC §4.1/§4.3). This image bundles
# the worker binary and the external tooling it shells out to, all resolved from
# the one pinned nixpkgs the whole flake shares — the *same versions* the static
# recovery-binary set (nix/recovery-binaries.nix, #72) stages onto the recovery
# disc. That single source of truth is the point: backup tooling and recovery
# tooling can never drift (SPEC §2/§4.1/§10).
#
# Built with `streamLayeredImage` (per media-processor): it emits a script that
# streams the image tarball to stdout, so `make build-images` pipes it straight
# into `docker load` without materialising a large tarball in the store.
{
  lib,
  dockerTools,
  buildEnv,
  # First-party inputs threaded from the flake.
  worker,
  ltfs,
  # External tooling — pinned by the shared nixpkgs.
  age,
  par2cmdline-turbo,
  zstd,
  gnutar,
  mt-st,
  mtx,
  sg3_utils,
  lsscsi,
  zfs,
  fuse,
  coreutils,
  util-linux,
  bashInteractive,
  cacert,
  # The static packages the recovery-binary set stages. Passed in so the version
  # equality that "same nixpkgs" already guarantees is asserted explicitly and
  # fails the build loudly on any drift (AC5).
  pkgsStatic,
  # Short nixpkgs revision, used as the deterministic image tag.
  nixpkgsRev,
}:
let
  # AC5: the image and the recovery disc must bundle identical tool versions.
  # Both come from the same pinned nixpkgs, so these already match; asserting it
  # turns "trust the pin" into a build-time guarantee. The static build (musl) of
  # a tool carries the same upstream `version` as its dynamic counterpart.
  versionMatches = {
    age = age.version == pkgsStatic.age.version;
    par2 = par2cmdline-turbo.version == pkgsStatic.par2cmdline-turbo.version;
    zstd = zstd.version == pkgsStatic.zstd.version;
    tar = gnutar.version == pkgsStatic.gnutar.version;
  };
  mismatches = lib.filterAttrs (_: matched: !matched) versionMatches;
  assertMsg =
    "data-worker image tool versions must match the recovery-binary set "
    + "(single pinned source of truth, SPEC §4.1/§10); drift in: "
    + lib.concatStringsSep ", " (lib.attrNames mismatches);

  # The tools the worker invokes at run time. Assembled into one env so they all
  # land under /bin with a single PATH entry.
  runtimeTools = [
    worker
    ltfs # mkltfs / ltfs / ltfsck / unltfs — LTFS format, mount, write (SPEC §6)
    age # >= 1.3.1, native post-quantum recipients (SPEC §7)
    par2cmdline-turbo # per-archive PAR2 recovery sets (SPEC §8)
    zstd # optional pre-encryption compression (SPEC §4.3)
    gnutar # archive creation (SPEC §4.3); recovery disc stages the same `tar`
    mt-st # `mt` — drive control on the non-rewinding nst nodes
    mtx # media-changer moves (SPEC §3)
    sg3_utils # sg_logs (TapeAlert / log pages), sg_turs (readiness) (SPEC §3)
    lsscsi # enumerate SCSI tape/changer devices
    zfs # `zfs get` — the data-side Resolve/Verify/Prepare activities read
    # dataset properties (logicalreferenced, mountpoint, user properties) and
    # locate the .zfs/snapshot/ tree through the zfs CLI (pkg/zfs). It needs
    # /dev/zfs passed through at run time (read-only ioctls; no `zfs create`).
    fuse # `fusermount` — reference LTFS is FUSE-based; pkg/ltfs unmounts a
    # written volume with `fusermount -u` (mount uses libfuse directly). Same
    # fuse (v2) that `ltfs` links against, so the userspace helper matches.
    coreutils # basic file utilities the activities and LTFS scripts rely on
    util-linux # mount/umount and friends for the FUSE LTFS path
    bashInteractive # a shell for ltfs/mtx helper invocations
  ];

  toolEnv = buildEnv {
    name = "data-worker-tools";
    paths = runtimeTools;
  };
in
assert lib.assertMsg (mismatches == { }) assertMsg;
dockerTools.streamLayeredImage {
  name = "tape-archiver-data-worker";
  # Deterministic, content-anchored tag (the nixpkgs rev that pins every tool),
  # never a floating `latest` — the operator re-tags for a registry in the
  # Makefile push flow.
  tag = nixpkgsRev;

  contents = [
    toolEnv
    cacert # TLS roots for the Temporal control-plane and Discord webhook
  ];

  # LTFS is FUSE-based and the write path needs writable scratch: create the
  # mount point for the /mnt/bulk-pool-01 bind mount, the staging parent, and a
  # world-writable /tmp. Devices (/dev/nst*, /dev/sch0, /dev/sg*, /dev/fuse) are
  # passed through at run time, not baked in.
  extraCommands = ''
    mkdir -p mnt/bulk-pool-01 tmp
    chmod 1777 tmp
  '';

  config = {
    Entrypoint = [ "/bin/worker" ];
    Env = [
      "PATH=/bin"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
    ];
    # Record the pinned versions on the image itself, so `docker inspect` reports
    # exactly what a run used without starting the container.
    Labels = {
      "org.opencontainers.image.title" = "tape-archiver-data-worker";
      "org.opencontainers.image.description" =
        "tape-archiver data worker: tar/age/PAR2/LTFS backup tooling (SPEC §4.1)";
      "io.tape-archiver.nixpkgs-rev" = nixpkgsRev;
      "io.tape-archiver.version.ltfs" = ltfs.version;
      "io.tape-archiver.version.age" = age.version;
      "io.tape-archiver.version.par2" = par2cmdline-turbo.version;
      "io.tape-archiver.version.zstd" = zstd.version;
      "io.tape-archiver.version.tar" = gnutar.version;
    };
  };
}
