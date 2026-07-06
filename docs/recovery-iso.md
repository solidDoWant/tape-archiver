# Recovery ISO (optical kit)

A run produces a single **ISO 9660 image** (SPEC §10): the self-contained optical
recovery kit. Together with the physical tapes it lets a future operator read, repair,
decrypt, decompress, and unpack the archives with nothing but the disc and the tapes —
no online services, no package manager, no original host. It is built by
`pkg/recoverykit`, and **only when optical burning is enabled**
([`delivery.opticalBurn`](configuration.md#opticalburn)) — as the mountable image the
Burn phase burns to each disc. The burned disc is the ISO's durable home, so a run
without burning produces no ISO, and the ISO is never uploaded to Discord; only the PDF
report is delivered (SPEC §11).

**Target media: M-DISC DVD.** Its inorganic recording layer is ISO/IEC 10995-tested and
NIST-listed for 100+ year archival life, and it is readable in the large, long-lived
installed base of DVD drives. The image is tens of MB, so DVD capacity is ample.

**Burning and read-back verification** can run **in the workflow** — configure
[`delivery.opticalBurn`](configuration.md#opticalburn) with the burner drives and copy
count and the Burn phase burns each disc and verifies it against the disc-content
manifest, pausing for the operator between burn-sets and on any failure (see
[configuration.md](configuration.md#optical-burn-operator-loop)). Left unconfigured,
burning stays a **manual operator step** (burn **at least two copies** and verify each
against the manifest). Either way, periodic re-burn/refresh is a documented maintenance
task.

## Contents

The image holds, at these paths:

- `report.pdf` — the PDF run report (SPEC §9). It embeds the contents manifest, the
  build parameters, the recovery procedure, and the age private identity.
- `manifest.sha256` — the full SHA-256 manifest covering every on-tape file.
- `recovery.txt` — the written, step-by-step recovery procedure (including LTFS read
  instructions).
- `ltfs-index/<barcode>.schema` — a backup copy of each tape's LTFS index, one per tape,
  named by the tape barcode. This lets the tape be read even if its on-tape index
  partition is damaged.
- `bin/<name>` — the static recovery binaries (`age`, `par2`, `zstd`, `tar`) staged from
  a configurable source directory.

File names are stored as ISO 9660 level-2 identifiers (lowercased, no Rock Ridge), which
the short, fixed artifact names above survive unchanged.

## Recovery binaries must be statically linked

The bundled binaries are the only tooling a recoverer is guaranteed to have. At restore
time — potentially decades later, on unknown hardware with no package manager — a
dynamically linked binary whose shared libraries cannot be resolved is dead weight.

`recoverykit.Build` therefore **inspects every staged binary and fails the run** if any
is not a statically linked native (ELF) executable: a binary that declares a program
interpreter (`PT_INTERP`) or any shared-library dependency (`DT_NEEDED`) is rejected. A
misconfigured run can never silently produce a useless recovery disc (SPEC §2:
20-year recoverability; everything is tested).

`recoverykit` itself only stages whatever is in the configured source directory and
proves it is static — it performs no network fetch at build or run time. The binaries
are produced separately, at the **same versions** shipped on the recovery disc and used
to write the tapes (see below).

## Producing the static binaries

The static set is a Nix derivation (`nix/recovery-binaries.nix`, exposed as the flake
output `recoveryBinaries`), built with:

```
make recovery-binaries      # nix build .#recoveryBinaries
```

It emits one disc-staging directory:

- `bin/{age,par2,zstd,tar}` — the statically linked binaries. This is the directory a
  run points `recoverykit.Build` at (`BinariesDir`, wired from the data worker's
  `TAPE_RECOVERY_BINARIES_DIR`); `recoverykit` stages its top-level regular files into
  the ISO's `bin/`.
- `src/<tool>-<version>.*` — each tool's upstream source archive (SPEC §10 "…plus their
  source"), staged for later inclusion on the disc. It lives in a subdirectory, which
  `recoverykit` skips, so it never trips the ELF-only linkage check on `bin/`.

`par2cmdline-turbo`, `zstd`, and `gnutar` are built with Nix `pkgsStatic` (musl); `age`
is Go and links static with CGO disabled. All four are drawn from the **same pinned
nixpkgs** as the rest of the project — the single shared source of truth — so the
data-worker OCI image, which bundles the same tools for the write path, ships identical
versions ("must match the recovery disc", SPEC §2/§4.1/§10). The derivation's install
check re-proves, at build time, the same predicate `recoverykit.Build` enforces at run
time (no `PT_INTERP`, no `DT_NEEDED`) and that each binary runs standalone at its pinned
version — including `age`'s native post-quantum (`age1pq1…`, hybrid ML-KEM-768)
support, which needs no separate plugin binary.

## Key escrow

The embedded `report.pdf` contains the age private identity (`AGE-SECRET-KEY-PQ-1…`).
This is the documented key-escrow decision (SPEC §7): the holder of the disc can always
decrypt the archives. The consequence, stated plainly: the recovery ISO (and the burned
disc) contains the decryption secret and **must be handled accordingly**. The delivered
PDF report likewise carries the identity, so its Discord delivery is equally sensitive.
See [report.md](report.md) for the full rationale.

## Implementation notes

- The image is written with the pure-Go
  [`github.com/kdomanski/iso9660`](https://github.com/kdomanski/iso9660) writer. Pure Go
  keeps the build and its tests hermetic with no runtime CLI dependency, in line with the
  20-year-recoverability principle (SPEC §2) and matching `pkg/report`'s choice.
- The PDF report and SHA-256 manifest are consumed as **input bytes**, so `pkg/recoverykit`
  has no compile-time dependency on `pkg/report` and can be built and tested with fixture
  inputs.
- Contents are verified in tests by reading the built image back with the same pure-Go
  reader and asserting every artifact is present at its expected path with its exact
  bytes, so the test exercises the real image rather than trusting the writer.
- The Report phase stages the image uncompressed as `recovery.iso` for the Burn phase to
  burn; it is not compressed, and — since the disc is the ISO's durable home — it is not
  delivered to Discord.
