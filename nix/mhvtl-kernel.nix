# Nix derivation for the mhvtl out-of-tree kernel module.
# Call via pkgs.linuxPackages.callPackage so `kernel` is automatically bound
# to the running kernel's package set.
{
  lib,
  stdenv,
  fetchFromGitHub,
  kernel,
}:
stdenv.mkDerivation (finalAttrs: {
  pname = "mhvtl-kernel";
  version = "1.8.0";

  src = fetchFromGitHub {
    owner = "markh794";
    repo = "mhvtl";
    rev = "8e79aa898381797afac596160dae2b74533e426d";
    hash = "sha256-L6suy9PZZvPcDFyD/G/v4IGZr/+obnpqqXldzlBJHFQ=";
  };

  patches = [
    # Fix use-after-free race in the VTL_PUT_DATA ioctl path (kernel >= 6.18).
    # Without this the st driver keeps drives in DR_OPEN after mtx load.
    ./patches/0001-mhvtl-fix-ioctl-use-after-free-race.patch
  ];

  nativeBuildInputs = kernel.moduleBuildDependencies;

  # mhvtl's kernel/Makefile uses KDIR to locate the kernel build tree.
  makeFlags = [
    "KDIR=${kernel.dev}/lib/modules/${kernel.modDirVersion}/build"
  ];

  preBuild = ''
    # config.sh uses #!/usr/bin/env bash which is unavailable in the Nix
    # sandbox.  Patch the shebang before make invokes it.
    patchShebangs kernel/config.sh
  '';

  buildPhase = ''
    runHook preBuild
    make -C kernel KDIR=${kernel.dev}/lib/modules/${kernel.modDirVersion}/build
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -D kernel/mhvtl.ko \
      "$out/lib/modules/${kernel.modDirVersion}/kernel/drivers/scsi/mhvtl.ko"
    runHook postInstall
  '';

  meta = {
    description = "mhvtl virtual tape library — kernel module";
    homepage = "https://github.com/markh794/mhvtl";
    license = lib.licenses.gpl2Only;
    platforms = [ "x86_64-linux" ];
  };
})
