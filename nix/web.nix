# Nix build of the tape-archiver web UI binary (`cmd/web`).
#
# cmd/web serves the built React SPA via go:embed (cmd/web/assets.go embeds
# cmd/web/dist), so the frontend must be built and copied into cmd/web/dist
# before `go build` runs. `make build`/`bin/web` does this out of band via
# `make frontend-build` (npm on the host); this derivation instead depends on
# the hermetic Nix build of the frontend (nix/web-frontend.nix) so `nix build
# .#web`/`.#webImage` never touches the network and never depends on a
# developer having already run `npm run build` locally.
{
  lib,
  buildGoModule,
  webFrontend,
}:
buildGoModule {
  pname = "tape-archiver-web";
  # Version is sourced from the module path / VCS at build time via the Go build
  # info the binary already reads (internal/buildinfo.ToolVersion); this attr is
  # only the derivation label.
  version = "0-dev";

  # The repository root. `lib.fileset` keeps the Nix store copy to the Go sources
  # and module metadata so unrelated edits (docs, nix, scripts) do not bust the
  # build cache or the vendor hash. cmd/web/dist is deliberately excluded here —
  # postPatch below replaces it wholesale with webFrontend's output, so whatever
  # (if anything) happens to be checked out locally in that gitignored directory
  # never leaks into the Nix build.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      (lib.fileset.difference ../cmd ../cmd/web/dist)
      ../internal
      ../pkg
      ../workflows
    ];
  };

  # Pinned vendor hash: refresh with `make update-dependencies` (or by setting
  # this to lib.fakeHash and reading the value nix reports on the failed build)
  # whenever go.mod/go.sum change. Shared Go module graph with nix/worker.nix,
  # so this is expected to track that derivation's vendorHash in lockstep.
  vendorHash = "sha256-0M0WKVBYzY2W1lnWG6w0AWNi8bZKlRwyoxpKxv1gxjA=";

  # Replace the gitignored (or, in a from-scratch checkout, placeholder-only)
  # cmd/web/dist with the hermetically-built frontend before compiling, so
  # go:embed (all:dist, cmd/web/assets.go) bundles the real SPA rather than the
  # committed dist/.gitkeep placeholder.
  postPatch = ''
    rm -rf cmd/web/dist
    cp -r ${webFrontend} cmd/web/dist
  '';

  subPackages = [ "cmd/web" ];

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
  ];

  # cmd/web's unit tests either need a live Temporal (integration-tagged, see
  # make test-integration) or are already covered by `make test` in CI; not
  # re-run as part of the hermetic image build.
  doCheck = false;

  meta = {
    description = "tape-archiver web UI HTTP server (embedded SPA + JSON API)";
    mainProgram = "web";
    platforms = [ "x86_64-linux" ];
  };
}
