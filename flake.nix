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
      in
      {
        # Expose as flake packages so `nix build .#mhvtl`, `.#mhvtlKernel`,
        # `.#zfs`, and `.#zfsKernel` work outside the dev shell.
        packages = {
          mhvtl = mhvtlUserspace;
          mhvtlKernel = mhvtlKernel;
          zfs = zfsUserspace;
          zfsKernel = zfsKernel;
          default = mhvtlUserspace;
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
            pkgs.gnumake
            pkgs.kubernetes-helm
            pkgs.gh

            # Tape library / changer tools (real hardware and virtual alike)
            pkgs.mtx
            pkgs.mt-st
            # SCSI generic tooling: sg_logs (drive log pages / TapeAlert),
            # sg_turs (TEST UNIT READY readiness probe), etc.
            pkgs.sg3_utils
            pkgs.lsscsi

            # mhvtl virtual tape library — userspace daemons and utilities
            mhvtlUserspace

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
