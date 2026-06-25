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
      in
      {
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
            pkgs.gnumake
            pkgs.kubernetes-helm
            pkgs.gh
            pkgs.docker
            pkgs.docker-compose

            # Teleport 18. Only `tsh` is needed, but nixpkgs ships Teleport as a
            # single monolithic package (tsh/tctl/tbot/teleport) with no
            # tsh-only derivation, so we pull in the whole thing.
            pkgs.teleport_18
          ];
        };
      });
}
