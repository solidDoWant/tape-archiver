# Nix build of the web/ frontend (Vite + React + TypeScript SPA) into a plain
# directory of static assets, hermetically — no network access at build time,
# per docs/web-ui-design.md §4 ("npm with a committed lockfile (Nix
# buildNpmPackage-compatible)"). nix/web.nix copies this output into
# cmd/web/dist/ before compiling the `web` binary, so go:embed bundles the
# exact same build `make frontend-build` (Makefile, npm directly) produces —
# same package-lock.json, same Vite config — just fetched/built by Nix instead
# of an ad hoc `npm ci && npm run build` on the host.
{
  lib,
  buildNpmPackage,
  nodejs_24,
}:
buildNpmPackage {
  pname = "tape-archiver-web-frontend";
  version = "0-dev";

  # Only web/ is needed to build the SPA.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = ../web;
  };
  sourceRoot = "source/web";

  nodejs = nodejs_24;

  # Pinned npm dependency hash: refresh with `make update-dependencies` (or by
  # setting this to lib.fakeHash and reading the value nix reports on the
  # failed build) whenever web/package-lock.json changes.
  npmDepsHash = "sha256-0YJyAKTrfADW+dHV+CEUOc3t4uwEo4FsgaTE+SlXRm4=";

  # `npm run build` runs `tsc -b && vite build` (web/package.json).
  # web/vite.config.ts hardcodes build.outDir to ../cmd/web/dist (relative to
  # web/ — see that file's doc comment: go:embed can only reach cmd/web's own
  # directory subtree) so that `make frontend-build` (npm on the host) lands
  # output where cmd/web/assets.go embeds it. That path resolves to a sibling
  # of this derivation's writable source root, which the Nix sandbox does not
  # grant write access to (only the declared sourceRoot, "source/web", is
  # chmod u+w after unpacking) — so this build instead overrides Vite's outDir
  # via its CLI flag (which takes precedence over vite.config.ts) to a plain
  # "dist" directory inside the writable source root; nix/web.nix then copies
  # this derivation's $out to cmd/web/dist itself before compiling `cmd/web`.
  npmBuildFlags = [
    "--" # forces npm to pass the following flags through to `vite build` itself
    "--outDir"
    "dist"
  ];

  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp -r dist/. "$out/"
    runHook postInstall
  '';

  # No hermetic unit-test target here — vitest (make test/frontend-test) and
  # eslint/tsc (make lint/frontend-lint) already cover this source in CI; this
  # derivation only needs to reproduce the production build output.
  doCheck = false;

  meta = {
    description = "tape-archiver web UI static frontend build (embedded into cmd/web)";
    platforms = [ "x86_64-linux" ];
  };
}
