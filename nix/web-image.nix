# Reproducible OCI image for the web UI (docs/web-ui-design.md §5).
#
# cmd/web is a standalone, stateless HTTP server (SPA + JSON API, gated behind
# OIDC — pkg/webauth) that talks only to the Temporal frontend and the OIDC
# identity provider; it never touches tape hardware or bulk data. So, like
# nix/control-worker-image.nix, this image is deliberately minimal: the `web`
# binary, the TLS roots it needs to reach Temporal/the IdP, and `age` — the one
# external tool cmd/web itself shells out to, for the config builder's
# server-side age-keygen endpoint (see the `age` param doc below).
#
# Unlike `worker`, `web` ships no `healthcheck` self-probe subcommand (adding
# one is out of scope for the Helm-chart/image packaging issue that introduced
# this file — cmd/web application code is a declared non-goal there); the
# container HEALTHCHECK below instead curls its own /healthz directly, per
# docs/configuration.md's "its container image can curl/equivalent its own
# /healthz endpoint directly" note. curlMinimal is bundled solely for that
# purpose — the worker images deliberately ship neither curl nor wget because
# they use the `healthcheck` subcommand instead; don't add curl there too.
{
  dockerTools,
  # First-party input threaded from the flake — the web UI server binary.
  web,
  # TLS roots — pinned by the shared nixpkgs.
  cacert,
  # Minimal curl build, used only for the container HEALTHCHECK (see above).
  curlMinimal,
  # age (>= 1.3.1, native post-quantum recipients — the same package the
  # data-worker image bundles, SPEC §7). Needed at runtime by the config
  # builder's server-side age keygen endpoint (POST /api/age/keygen, issue
  # #279), which shells out to `age-keygen -pq` (pkg/agewrap.GenerateIdentity):
  # without it on PATH the endpoint fails with "could not generate an age
  # keypair" and the guided Form-mode keypair generation is dead in every
  # deployment.
  age,
  # Short nixpkgs revision, used as the deterministic image tag.
  nixpkgsRev,
}:
dockerTools.streamLayeredImage {
  name = "tape-archiver-web";
  # Deterministic, content-anchored tag (the nixpkgs rev that pins the build),
  # never a floating `latest` — the operator re-tags for a registry in the
  # Makefile push flow.
  tag = nixpkgsRev;

  contents = [
    web
    cacert # TLS roots for the Temporal frontend and the OIDC identity provider
    curlMinimal # HEALTHCHECK only — see file doc comment
    age # age-keygen for POST /api/age/keygen (issue #279) — see param doc above
  ];

  config = {
    Entrypoint = [ "/bin/web" ];
    Env = [
      "PATH=/bin"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
    ];
    # Container health via a direct /healthz probe (liveness semantics: 200 as
    # soon as the process is serving, independent of Temporal connectivity —
    # see pkg/health). This deliberately DIFFERS from the worker images'
    # Healthcheck (nix/control-worker-image.nix), which probes /readyz and so
    # reflects readiness — a worker that has lost its Temporal connection
    # reports unhealthy there. This image has no such self-probe subcommand
    # (see file doc comment above) and so falls back to a plain curl against
    # /healthz, which cannot reflect Temporal connectivity. Kubernetes' own
    # httpGet readiness probe (deploy/charts/tape-archiver-web) is what
    # actually gates traffic on Temporal connectivity for this image; the
    # container-level Healthcheck here only proves the process is alive and
    # serving. Durations are in nanoseconds per the OCI image config schema.
    # Port 8081 matches cmd/web's HEALTH_ADDR default (docs/configuration.md);
    # deployments overriding HEALTH_ADDR must also override this Healthcheck
    # (e.g. via Helm chart container overrides).
    Healthcheck = {
      Test = [
        "CMD"
        "/bin/curl"
        "-f"
        "http://127.0.0.1:8081/healthz"
      ];
      Interval = 10000000000; # 10s
      Timeout = 3000000000; # 3s
      Retries = 3;
    };
    # Record the pinned revision on the image itself, so `docker inspect` reports
    # exactly what a run used without starting the container.
    Labels = {
      "org.opencontainers.image.title" = "tape-archiver-web";
      "org.opencontainers.image.description" =
        "tape-archiver web UI: HTTP server for the browser SPA + JSON API, OIDC-gated (docs/web-ui-design.md)";
      "io.tape-archiver.nixpkgs-rev" = nixpkgsRev;
    };
  };
}
