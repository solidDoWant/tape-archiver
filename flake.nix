{
  description = "tape-archiver development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        mhvtlUserspace = pkgs.callPackage ./nix/mhvtl-userspace.nix { };

        # Reference open-source LTFS (mkltfs/ltfs/ltfsck) — pkg/ltfs shells out
        # to these to format, mount, and unmount tape volumes, and the same
        # binaries ship on the recovery disc (SPEC §6, §10). The reference
        # LinearTape-Open implementation is used rather than the vendor-locked
        # hpe-ltfs in nixpkgs: hpe-ltfs refuses non-HPE drives (so it cannot be
        # tested against the mhvtl IBM-emulated drive) and the production drives
        # are IBM (issue #12, SPEC §16).
        ltfs = pkgs.callPackage ./nix/ltfs.nix { };

        # Kernel module built against the same kernel that NixOS boots by
        # default (pkgs.linuxPackages).  The VM's configuration.nix sets
        # kernelPackages = pkgs.linuxPackages, so the .ko produced here
        # matches the running kernel.
        mhvtlKernel = pkgs.linuxPackages.callPackage ./nix/mhvtl-kernel.nix { };

        # ZFS userspace tools (zpool, zfs, zdb) and the matching kernel module,
        # both pinned by the same nixpkgs revision so their OpenZFS versions are
        # identical (a userspace/module version mismatch makes zpool refuse to
        # talk to the module). linuxPackages.zfs was removed upstream; the
        # per-version module attribute is selected via kernelModuleAttribute
        # (e.g. "zfs_2_4"). Like mhvtlKernel, the module is built against
        # pkgs.linuxPackages — the kernel the dev VM boots — so it loads into the
        # running kernel.
        zfsUserspace = pkgs.zfs;
        zfsKernel = pkgs.linuxPackages.${zfsUserspace.kernelModuleAttribute};

        # Static recovery-binary set for the optical recovery disc (SPEC §10):
        # statically linked age/par2/zstd/tar plus their source, staged into one
        # directory. Built from the same pinned nixpkgs as everything else, so
        # the data-worker image bundles identical versions ("must match the
        # recovery disc", SPEC §2/§4.1/§10). recoverykit.Build consumes the
        # bin/ subdirectory as its BinariesDir.
        recoveryBinaries = pkgs.callPackage ./nix/recovery-binaries.nix { };

        # The Temporal worker binary (control/data — the role is a run-time env
        # var, SPEC §4.1). Built once here and bundled into the data-worker OCI
        # image below.
        worker = pkgs.callPackage ./nix/worker.nix { };

        # Reproducible OCI image for the data worker (SPEC §4.1): the worker
        # binary plus its external tooling, all from this same pinned nixpkgs and
        # therefore the identical versions the recovery-binary set stages onto
        # the disc (issue #73, SPEC §2/§10). streamLayeredImage emits a script
        # that streams the tarball into `docker load` (see `make build-images`).
        dataWorkerImage = pkgs.callPackage ./nix/data-worker-image.nix {
          inherit worker ltfs;
          inherit (pkgs) pkgsStatic;
          nixpkgsRev = nixpkgs.shortRev or "dirty";
        };

        # Reproducible OCI image for the control worker (SPEC §4.1): the same
        # shared `worker` binary as the data-worker image above, plus TLS roots
        # only — no tape/bulk-data tooling. The control worker orchestrates runs
        # in Kubernetes on the `control` queue (issue #77). streamLayeredImage
        # emits a script that streams the tarball into `docker load` (see `make
        # build-images`).
        controlWorkerImage = pkgs.callPackage ./nix/control-worker-image.nix {
          inherit worker;
          nixpkgsRev = nixpkgs.shortRev or "dirty";
        };
      in
      {
        # Expose as flake packages so `nix build .#mhvtl`, `.#mhvtlKernel`,
        # `.#zfs`, and `.#zfsKernel` work outside the dev shell.
        packages = {
          mhvtl = mhvtlUserspace;
          mhvtlKernel = mhvtlKernel;
          zfs = zfsUserspace;
          zfsKernel = zfsKernel;
          inherit ltfs;
          inherit recoveryBinaries;
          inherit worker;
          inherit dataWorkerImage;
          inherit controlWorkerImage;
          default = mhvtlUserspace;
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
            pkgs.gnumake
            pkgs.kubernetes-helm
            pkgs.gh

            # age (>= 1.3.1, native post-quantum recipients) — pkg/agewrap shells
            # out to `age` to encrypt archives and the tests use `age-keygen -pq`.
            # The same binary and version ship on the recovery disc (SPEC §7).
            pkgs.age

            # Tape library / changer tools (real hardware and virtual alike)
            pkgs.mtx
            pkgs.mt-st
            # SCSI generic tooling: sg_logs (drive log pages / TapeAlert),
            # sg_turs (TEST UNIT READY readiness probe), etc.
            pkgs.sg3_utils
            pkgs.lsscsi

            # PAR2 error correction (par2cmdline-turbo) — pkg/par2 shells out to
            # `par2` to build per-archive recovery sets and the tests verify and
            # repair with it. The same binary ships on the recovery disc
            # (SPEC §8, §10).
            pkgs.par2cmdline-turbo

            # mhvtl virtual tape library — userspace daemons and utilities
            mhvtlUserspace

            # Reference LTFS (mkltfs/ltfs/ltfsck) — pkg/ltfs formats, mounts, and
            # unmounts tape volumes through these; the integration tests exercise
            # the full path against mhvtl. Same binaries ship on the recovery disc.
            ltfs

            # ZFS userspace tools (zpool, zfs, zdb). pkg/zfs shells out to `zfs`
            # and the integration-test harness creates an ephemeral file-backed
            # pool with `zpool`. The matching kernel module is built above as
            # zfsKernel and exposed via $ZFS_MODULES below; zpool-up.sh depmods
            # it into a temp tree and loads it at runtime (no system config, no
            # reboot), falling back to the host's own module when the flake build
            # does not match the running kernel (e.g. the Ubuntu storage host's
            # DKMS module). Loading still needs root — that is inherent to kernel
            # modules, not something the flake can grant.
            zfsUserspace

            pkgs.docker
            pkgs.docker-compose

            # Teleport 18. Only `tsh` is needed, but nixpkgs ships Teleport as a
            # single monolithic package (tsh/tctl/tbot/teleport) with no
            # tsh-only derivation, so we pull in the whole thing.
            pkgs.teleport_18

            # Temporal CLI — inspect and interact with the local dev stack from
            # the host: `temporal workflow list`, `temporal operator cluster
            # health`, etc. The version is pinned to match the server image in
            # docker-compose.yml so the client/server wire protocol stays in sync.
            pkgs.temporal-cli
          ];

          # Expose the built kernel modules so the up-scripts can load them
          # without re-invoking nix: mhvtl-up.sh insmods $MHVTL_KO directly (a
          # single dependency-free .ko); zpool-up.sh depmods the $ZFS_MODULES
          # tree (spl, zfs, and friends) into a temp dir and modprobes from
          # there, since ZFS is a multi-module dependency graph.
          shellHook = ''
            export MHVTL_KO="${mhvtlKernel}/lib/modules/$(ls ${mhvtlKernel}/lib/modules)/kernel/drivers/scsi/mhvtl.ko"
            export ZFS_MODULES="${zfsKernel}"
          '';
        };
      });
}
