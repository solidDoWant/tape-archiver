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
      in
      {
        # Expose as flake packages so `nix build .#mhvtl` and
        # `nix build .#mhvtlKernel` work outside the dev shell.
        packages = {
          mhvtl = mhvtlUserspace;
          mhvtlKernel = mhvtlKernel;
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

            pkgs.docker
            pkgs.docker-compose

            # Teleport 18. Only `tsh` is needed, but nixpkgs ships Teleport as a
            # single monolithic package (tsh/tctl/tbot/teleport) with no
            # tsh-only derivation, so we pull in the whole thing.
            pkgs.teleport_18
          ];

          # Expose the path to the built kernel module so mhvtl-up.sh can
          # load it with `sudo insmod "$MHVTL_KO"` without re-invoking nix.
          shellHook = ''
            export MHVTL_KO="${mhvtlKernel}/lib/modules/$(ls ${mhvtlKernel}/lib/modules)/kernel/drivers/scsi/mhvtl.ko"
          '';
        };
      });
}
