# Recovery ISO (optical kit)

Every run produces a single **ISO 9660 image** (SPEC §10): the self-contained optical
recovery kit. Together with the physical tapes it lets a future operator read, repair,
decrypt, decompress, and unpack the archives with nothing but the disc and the tapes —
no online services, no package manager, no original host. It is built by
`pkg/recoverykit` and delivered alongside the report (SPEC §11).

**Target media: M-DISC DVD.** Its inorganic recording layer is ISO/IEC 10995-tested and
NIST-listed for 100+ year archival life, and it is readable in the large, long-lived
installed base of DVD drives. The image is tens of MB, so DVD capacity is ample. Burning
and read-back verification are manual operator steps (burn **at least two copies** and
verify each against the manifest); periodic re-burn/refresh is a documented maintenance
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

The binaries themselves are produced and pinned elsewhere (the worker OCI image), at the
**same versions** shipped on the recovery disc and used to write the tapes. `recoverykit`
only stages whatever is in the configured source directory and proves it is static — it
performs no network fetch at build or run time.

## Key escrow

The embedded `report.pdf` contains the age private identity (`AGE-SECRET-KEY-PQ-1…`).
This is the documented key-escrow decision (SPEC §7): the holder of the disc can always
decrypt the archives. The consequence, stated plainly: the recovery ISO (and the Discord
delivery that carries it) contains the decryption secret and **must be handled
accordingly**. See [report.md](report.md) for the full rationale.

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
- Compression of the `.iso` (SPEC §11) is handled by the Report phase, which zstd-
  compresses the image as it is built and hands the compressed artifact to the Deliver
  phase; `pkg/recoverykit` itself emits an uncompressed image.
