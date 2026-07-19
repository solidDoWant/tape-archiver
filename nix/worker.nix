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
  #
  # Deliberately `../cmd/worker`, not the whole `../cmd` tree (issue #259): this
  # is a single Go module, so `buildGoModule`'s vendorHash is a fixed-output hash
  # of `go mod vendor`'s result, which is computed from every package `go
  # list ./...` finds under the copied src — i.e. every package actually present
  # in the store copy, not just the one subPackages builds. Including cmd/web
  # and cmd/tapectl here would make this vendorHash a function of their imports
  # too, so a web- or tapectl-only dependency change would break `nix build
  # .#worker` with a fixed-output-hash mismatch despite cmd/worker's own
  # dependency tree being untouched. internal/pkg/workflows are genuinely shared
  # with cmd/worker, so they stay included — a dependency change there is a real
  # coupling, not an unrelated one.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd/worker
      ../internal
      ../pkg
      # pkg/runsapi imports the schemas Go package (issue #279), which go:embeds
      # the committed run-config JSON schema — the vendor step resolves imports
      # for every package in the store copy, so both the .go file and the .json
      # it embeds must be present.
      ../schemas
      ../workflows
    ];
  };

  # Pinned vendor hash: refresh with `make update-dependencies` (or by setting
  # this to lib.fakeHash and reading the value nix reports on the failed build)
  # whenever go.mod/go.sum, cmd/worker's own imports, or internal/pkg/workflows/
  # schemas' imports change. No longer shared with nix/web.nix's vendorHash
  # (#259): each binary's src fileset is now scoped to its own subPackage, so
  # the two hashes are independent, refreshed separately, and need not match
  # (this one is unchanged from before the split because cmd/web and cmd/tapectl
  # add no external imports beyond what the shared pkg/internal packages —
  # e.g. pkg/webauth, internal/testutil — already pull in).
  vendorHash = "sha256-6e9wQ4vXDJsvc6+xh7iy0cN6QZ2/AKvgPeciDZPx55U=";

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
