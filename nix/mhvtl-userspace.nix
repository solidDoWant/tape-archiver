# Nix derivation for the mhvtl userspace daemons and utilities:
# vtllibrary, vtltape, vtlcmd, make_vtl_media, mktape, and the
# libvtlscsi shared library they depend on.
{
  lib,
  stdenv,
  fetchFromGitHub,
  autoPatchelfHook,
  makeWrapper,
  zlib,
  gawk,
  coreutils,
  util-linux,
}:
stdenv.mkDerivation (finalAttrs: {
  pname = "mhvtl";
  version = "1.8.0";

  src = fetchFromGitHub {
    owner = "markh794";
    repo = "mhvtl";
    rev = "8e79aa898381797afac596160dae2b74533e426d";
    hash = "sha256-L6suy9PZZvPcDFyD/G/v4IGZr/+obnpqqXldzlBJHFQ=";
  };

  nativeBuildInputs = [
    autoPatchelfHook
    makeWrapper
  ];
  buildInputs = [ zlib ];

  # make_vtl_media is a shell script, not a binary: it shells out to awk to find
  # the libraries in device.conf and the barcodes in library_contents.<id>, to
  # sort/rm/mkdir/chmod for the media tree, to getopt for its own arguments, and
  # to mktape for each volume. None of that is declared anywhere, and it fails
  # open — with no awk on PATH both of its loops iterate over an empty list, so
  # it creates nothing and still exits 0 (issue #339).
  #
  # The dependency belongs to this derivation rather than to whatever consumes
  # it. The dev shell happened to satisfy it and the data-worker image did not,
  # which is precisely the drift a wrapper removes: the script now carries its
  # own PATH, so it behaves identically wherever it is bundled. $out/bin is on
  # it too, because find_mktape_path locates mktape by scanning PATH.
  makeVTLMediaPath = lib.makeBinPath [
    gawk
    coreutils
    util-linux
  ];

  buildPhase = ''
    runHook preBuild
    make -C usr \
      IFLAGS="-I ../include -I ../include/utils -I ../include/common -I ../ccan -I${zlib.dev}/include"
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/bin" "$out/lib"

    # Shared library — must land in $out/lib before autoPatchelfHook runs
    # so it can find it when fixing RPATH on the binaries below.
    install -m 755 usr/libvtlscsi.so "$out/lib/libvtlscsi.so"

    # Tell autoPatchelfHook to search $out/lib for same-derivation libraries.
    addAutoPatchelfSearchPath "$out/lib"

    # Binaries
    for bin in \
        dump_messageQ \
        dump_tape \
        edit_tape \
        make_vtl_media \
        mktape \
        tapeexerciser \
        validate_crc \
        vtlcmd \
        vtllibrary \
        vtltape; do
      install -m 755 "usr/bin/$bin" "$out/bin/$bin"
    done
    ln -s dump_tape "$out/bin/preload_tape"

    wrapProgram "$out/bin/make_vtl_media" \
      --prefix PATH : "$out/bin:${finalAttrs.makeVTLMediaPath}"

    # Config templates (for reference / scripting)
    mkdir -p "$out/share/mhvtl"
    install -m 644 etc/mhvtl.conf.in               "$out/share/mhvtl/"
    install -m 644 etc/library_contents.sample      "$out/share/mhvtl/"
    install -m 755 etc/generate_device_conf.in      "$out/share/mhvtl/generate_device_conf"
    install -m 755 etc/generate_library_contents.in "$out/share/mhvtl/generate_library_contents"

    runHook postInstall
  '';

  meta = {
    description = "mhvtl virtual tape library — userspace daemons and utilities";
    homepage = "https://github.com/markh794/mhvtl";
    license = lib.licenses.gpl2Only;
    platforms = [ "x86_64-linux" ];
  };
})
