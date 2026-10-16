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

  patches = [
    # Upstream bug (still in 2.4.9.0): switching a drive left in append-only mode
    # back to write-anywhere unloads the cartridge while LTFS holds PREVENT MEDIUM
    # REMOVAL, so mkltfs fails with EDEV_MEDIUM_LOCKED on real drives.
    ./patches/ltfs-0001-release-medium-lock-before-write-mode-unload.patch
    # Upstream bug (2.4.8.3 through 2.4.9.0, from 1f1ee3c): the volume coherency
    # MAM attribute's "LTFS" signature is written without its NUL into an
    # uninitialized buffer, so mounts reject it (LTFS12062W) and run a full
    # medium consistency check (issue #426; upstream PRs #623/#644, unmerged).
    ./patches/ltfs-0002-terminate-volume-coherency-signature.patch
  ];

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
