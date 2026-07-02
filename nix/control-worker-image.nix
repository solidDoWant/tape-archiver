# Reproducible OCI image for the control worker (SPEC §4.1).
#
# The control worker runs in Kubernetes and orchestrates runs on the `control`
# task queue: it drives the workflow, discovers snapshots, and posts failure
# notifications to Discord — but it never touches bulk data or tape hardware
# (SPEC §4.1: "Lightweight, no bulk data"). So this image is deliberately
# minimal: just the shared worker binary and the TLS roots it needs to reach the
# Temporal control plane and the Discord webhook. None of the tar/age/PAR2/LTFS/
# changer tooling the data-worker image (nix/data-worker-image.nix, #73) bundles
# belongs here.
#
# The `worker` derivation is the *same* one the data-worker image consumes — one
# shared `packages.worker` base — so both images run the byte-for-byte identical
# binary from the same pinned source (the `role` env var selects control vs data
# at run time, SPEC §4.1). Built with `streamLayeredImage` (per media-processor):
# it emits a script that streams the image tarball to stdout, so `make
# build-images` pipes it straight into `docker load`.
{
  dockerTools,
  # First-party input threaded from the flake — the shared worker base.
  worker,
  # TLS roots — pinned by the shared nixpkgs.
  cacert,
  # Short nixpkgs revision, used as the deterministic image tag.
  nixpkgsRev,
}:
dockerTools.streamLayeredImage {
  name = "tape-archiver-control-worker";
  # Deterministic, content-anchored tag (the nixpkgs rev that pins the build),
  # never a floating `latest` — the operator re-tags for a registry in the
  # Makefile push flow.
  tag = nixpkgsRev;

  contents = [
    worker
    cacert # TLS roots for the Temporal control-plane and Discord webhook
  ];

  config = {
    Entrypoint = [ "/bin/worker" ];
    Env = [
      "PATH=/bin"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
    ];
    # Record the pinned revision on the image itself, so `docker inspect` reports
    # exactly what a run used without starting the container.
    Labels = {
      "org.opencontainers.image.title" = "tape-archiver-control-worker";
      "org.opencontainers.image.description" =
        "tape-archiver control worker: lightweight Kubernetes orchestration, no tape tooling (SPEC §4.1)";
      "io.tape-archiver.nixpkgs-rev" = nixpkgsRev;
    };
  };
}
