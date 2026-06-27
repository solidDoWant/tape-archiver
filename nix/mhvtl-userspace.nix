# Nix derivation for the mhvtl userspace daemons and utilities:
# vtllibrary, vtltape, vtlcmd, make_vtl_media, mktape, and the
# libvtlscsi shared library they depend on.
{
  lib,
  stdenv,
  fetchFromGitHub,
  autoPatchelfHook,
  zlib,
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

  nativeBuildInputs = [ autoPatchelfHook ];
  buildInputs = [ zlib ];

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
