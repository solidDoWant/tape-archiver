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
  # The tape-archiver git revision, stamped into the binary via -ldflags so
  # internal/buildinfo.ToolVersion can report it at run time (the web UI shows it
  # as the app version). Empty by default; flake.nix passes the flake's
  # self.rev/self.dirtyRev. Necessary because the src below is a lib.fileset copy
  # with no .git directory, so the binary cannot recover the VCS revision from its
  # own embedded build info.
  version ? "",
}:
buildGoModule {
  pname = "tape-archiver-web";
  # The derivation label. The version the binary *reports* comes from the
  # -ldflags -X stamp below (or the Go build info for a plain `go build`), not
  # from this attr.
  version = if version != "" then version else "0-dev";

  # The repository root. `lib.fileset` keeps the Nix store copy to the Go sources
  # and module metadata so unrelated edits (docs, nix, scripts) do not bust the
  # build cache or the vendor hash. cmd/web/dist is deliberately excluded here —
  # postPatch below replaces it wholesale with webFrontend's output, so whatever
  # (if anything) happens to be checked out locally in that gitignored directory
  # never leaks into the Nix build.
  #
  # Deliberately `../cmd/web` (minus dist), not the whole `../cmd` tree (issue
  # #259): this is a single Go module, so `buildGoModule`'s vendorHash is a
  # fixed-output hash of `go mod vendor`'s result, which is computed from every
  # package `go list ./...` finds under the copied src — i.e. every package
  # actually present in the store copy, not just the one subPackages builds.
  # Including cmd/worker and cmd/tapectl here would make this vendorHash a
  # function of their imports too, so a worker- or tapectl-only dependency
  # change would break `nix build .#web` with a fixed-output-hash mismatch
  # despite cmd/web's own dependency tree being untouched. internal/pkg/
  # workflows/schemas are genuinely shared with cmd/web, so they stay included —
  # a dependency change there is a real coupling, not an unrelated one.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      (lib.fileset.difference ../cmd/web ../cmd/web/dist)
      ../internal
      ../pkg
      # The schemas Go package (issue #279) embeds the committed run-config
      # JSON schema for GET /api/config/schema — both the .go file and the
      # .json it go:embeds must be in the store copy.
      ../schemas
      ../workflows
    ];
  };

  # Refresh with `make update-dependencies` whenever the Go imports reachable
  # from cmd/web change. Independent of nix/worker.nix's vendorHash.
  vendorHash = "sha256-lXPKEyZNFEufurx1DNwrqEqZAsdZCbukgHAvHGLubfE=";

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
  ]
  ++ lib.optionals (version != "") [
    "-X github.com/solidDoWant/tape-archiver/internal/buildinfo.version=${version}"
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
