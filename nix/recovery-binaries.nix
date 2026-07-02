# Static recovery-binary set for the optical recovery disc (SPEC §10).
#
# Produces a single disc-staging directory holding statically linked `age`,
# `par2`, `zstd`, and `tar` — plus each tool's upstream source — so a future
# operator can decrypt, repair, decompress, and unpack the archives with nothing
# but the disc and the tapes, on unknown hardware with no package manager.
#
# Layout:
#   $out/bin/{age,par2,zstd,tar}   the static binaries (recoverykit.Build's
#                                  BinariesDir points here; it stages this dir's
#                                  top-level regular files into the ISO's /bin)
#   $out/src/<tool>-<version>.*    the matching upstream source archives (a
#                                  subdirectory recoverykit skips)
#
# Versions come from the flake's pinned nixpkgs — the single shared source of
# truth — so the sibling data-worker image, referencing the same `pkgs`, bundles
# identical versions (SPEC §2/§4.1/§10: "must match the recovery disc").
#
# `par2cmdline-turbo`, `zstd`, and `gnutar` are built with `pkgsStatic` (musl);
# `age` is Go and links static with CGO disabled. Every binary is proven static
# and standalone by the install-check phase below, mirroring the linkage check
# recoverykit.Build enforces at run time.
{
  lib,
  stdenvNoCC,
  pkgsStatic,
  binutils,
}:
let
  # Each tool: the static package, the single binary name to stage (the packages
  # ship extra binaries we deliberately drop, e.g. age-keygen, par2 symlinks,
  # zstdmt), and the substrings its `--version` output must contain. The version
  # substring is the package's own pinned version, so a nixpkgs bump that changes
  # the shipped version fails the build loudly rather than silently drifting from
  # the disc. par2 additionally must self-identify as par2cmdline-turbo.
  tools = [
    {
      drv = pkgsStatic.age;
      bin = "age";
      checks = [ pkgsStatic.age.version ];
    }
    {
      drv = pkgsStatic.par2cmdline-turbo;
      bin = "par2";
      checks = [ pkgsStatic.par2cmdline-turbo.version "par2cmdline-turbo" ];
    }
    {
      drv = pkgsStatic.zstd;
      bin = "zstd";
      checks = [ pkgsStatic.zstd.version ];
    }
    {
      drv = pkgsStatic.gnutar;
      bin = "tar";
      checks = [ pkgsStatic.gnutar.version ];
    }
  ];
in
stdenvNoCC.mkDerivation {
  name = "recovery-binaries";

  # Nothing to unpack — this derivation only assembles already-built inputs.
  dontUnpack = true;

  # readelf, to prove each staged binary is statically linked.
  nativeBuildInputs = [ binutils ];

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/bin" "$out/src"

    # Stage a tool's upstream source as an archive under $out/src. A git source
    # is a directory (re-archived deterministically); an upstream tarball is a
    # file (copied verbatim, with the nix store hash prefix stripped from its
    # name).
    stageSource() {
      local src="$1" name="$2"
      if [ -d "$src" ]; then
        tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
          -cf - -C "$src" . | gzip -n > "$out/src/$name.tar.gz"
      else
        local base="''${src##*/}"
        cp "$src" "$out/src/''${base#*-}"
      fi
    }

    ${lib.concatMapStrings (tool: ''
      install -Dm755 ${lib.getExe' tool.drv tool.bin} "$out/bin/${tool.bin}"
      stageSource ${tool.drv.src} "${tool.drv.pname}-${tool.drv.version}"
    '') tools}

    runHook postInstall
  '';

  # ACs 2 & 3, as hermetic build-time checks: every staged binary is a statically
  # linked native ELF (no PT_INTERP, no DT_NEEDED — the exact predicate
  # recoverykit.Build enforces) and runs standalone reporting its pinned version.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck

    checkBinary() {
      local tool="$1"; shift
      local bin="$out/bin/$tool"

      echo "checking $tool ($bin)"

      if readelf -l "$bin" | grep -qw INTERP; then
        echo "ERROR: recovery binary $tool declares a program interpreter (PT_INTERP); it must be statically linked" >&2
        exit 1
      fi

      if readelf -d "$bin" 2>/dev/null | grep -qw NEEDED; then
        echo "ERROR: recovery binary $tool declares shared-library dependencies (DT_NEEDED); it must be statically linked" >&2
        exit 1
      fi

      local version
      version="$("$bin" --version 2>&1 | head -1 || true)"
      echo "  --version: $version"

      local want
      for want in "$@"; do
        case "$version" in
          *"$want"*) ;;
          *)
            echo "ERROR: $tool --version output '$version' is missing expected substring '$want'" >&2
            exit 1
            ;;
        esac
      done
    }

    ${lib.concatMapStrings (
      tool: "checkBinary ${lib.escapeShellArgs ([ tool.bin ] ++ tool.checks)}\n"
    ) tools}

    runHook postInstallCheck
  '';

  meta = {
    description = "Static recovery-binary set (age, par2, zstd, tar) plus source for the recovery disc";
    platforms = [ "x86_64-linux" ];
  };
}
