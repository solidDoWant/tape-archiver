# Nix build of the tape-archiver Temporal worker binary (`cmd/worker`).
#
# One binary serves both roles: the `role` env var selects the control queue
# (Kubernetes) or the data queue (this image, on the storage host) at run time
# (SPEC §4.1). This derivation produces that binary; nix/data-worker-image.nix
# bundles it as the OCI image entrypoint alongside the pinned external tooling.
#
# CGO is disabled so the binary is a self-contained static Go executable with no
# libc dependency — it runs regardless of what (if anything) the base image
# provides. Built from the same pinned nixpkgs as every other derivation.
{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "tape-archiver-worker";
  # Version is sourced from the module path / VCS at build time via the Go build
  # info the binary already reads (internal/buildinfo.ToolVersion); this attr is
  # only the derivation label.
  version = "0-dev";

  # The repository root. `lib.fileset` keeps the Nix store copy to the Go sources
  # and module metadata so unrelated edits (docs, nix, scripts) do not bust the
  # build cache or the vendor hash.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ../pkg
      ../workflows
    ];
  };

  # Pinned vendor hash: refresh with `make update-dependencies` (or by setting
  # this to lib.fakeHash and reading the value nix reports on the failed build)
  # whenever go.mod/go.sum change.
  vendorHash = "sha256-i024+cndrL7aPwPP4UsVpyrNByo7b3EIlACu1fxMfx8=";

  subPackages = [ "cmd/worker" ];

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
  ];

  # The Temporal worker has no unit tests that run hermetically without a live
  # cluster; those are covered by the integration suite (make test-integration),
  # not the image build.
  doCheck = false;

  meta = {
    description = "tape-archiver Temporal worker (control/data) binary";
    mainProgram = "worker";
    platforms = [ "x86_64-linux" ];
  };
}
