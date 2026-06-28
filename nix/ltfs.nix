# Nix derivation for the reference open-source LinearTape-Open LTFS
# (single-drive ltfs: mkltfs, ltfs, ltfsck, unltfs). IBM-maintained,
# multi-vendor, Apache-2.0 — chosen over the vendor-locked hpe-ltfs because the
# production drives are IBM and the integration suite exercises an IBM-emulated
# mhvtl drive (see issue #12 / SPEC.md §16).
{
  lib,
  stdenv,
  fetchFromGitHub,
  autoreconfHook,
  pkg-config,
  fuse,
  libxml2,
  icu,
  libuuid,
}:
stdenv.mkDerivation (finalAttrs: {
  pname = "ltfs";
  version = "2.4.8.4";

  src = fetchFromGitHub {
    owner = "LinearTapeFileSystem";
    repo = "ltfs";
    rev = "v${finalAttrs.version}-10522";
    fetchSubmodules = true;
    hash = "sha256-E3qMdZL7UF/phUjayxLGBLpDG9rDXzG5cFECY+tlNlM=";
  };

  nativeBuildInputs = [
    autoreconfHook
    pkg-config
  ];

  buildInputs = [
    fuse
    libxml2
    icu
    libuuid
  ];

  # SNMP alert traps are irrelevant to this workflow (the worker reports through
  # Temporal/Discord, not SNMP); disabling drops the net-snmp dependency.
  configureFlags = [ "--disable-snmp" ];

  meta = {
    description = "Reference open-source Linear Tape File System (LTFS) — single-drive tools";
    homepage = "https://github.com/LinearTapeFileSystem/ltfs";
    license = lib.licenses.asl20;
    platforms = [ "x86_64-linux" ];
  };
})
