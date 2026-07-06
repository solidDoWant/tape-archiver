# Run Report (PDF)

Every run produces a single PDF report (SPEC §9): the durable, printable, laminated
**offline index** for that run. It is built by `pkg/report` from a complete run manifest
and is intended to be printed and stored alongside the physical tapes, and is also
embedded in the recovery ISO.

## Contents

The report contains, at minimum:

- **Run** — run id and date.
- **Contents manifest** — every archive, with its on-tape directory
  (`archives/NNN-<label>/`), member volumes, source snapshots, and per-file sizes and
  SHA-256 checksums.
- **Tapes** — which physical tape (by barcode/label) holds what. A tape that was written
  over a non-blank tape (because the run set [`library.allowNonBlankTapes`](configuration.md))
  is annotated here as `[Overwrote a non-blank tape]`, so a deliberate, irreversible
  overwrite is on the record.
- **Recovery discs** — when optical burning ran ([`delivery.opticalBurn`](configuration.md#opticalburn)),
  the burner each recovery disc was written on. A disc burned over a reclaimed non-blank
  rewritable disc is annotated `[Overwrote a non-blank disc]`, mirroring the tape overwrite
  note. The section is omitted entirely when no discs were burned. Because the Report phase
  runs before the Burn phase, this section appears only in the **delivered** report (re-rendered
  after the burn), not in the copy embedded in the burned ISO.
- **Write health** — per-tape, observational write-health from the run's tape-write
  window (see below).
- **Build parameters** — how the tapes were built: tool version, `age`/`par2`/`ltfs`
  versions, slice size, PAR2 redundancy, and the drive/library identifiers. The drive is
  recorded as its model, the **LTO generation required to read the tape** (the fact a
  future recoverer actually needs), and its serial; the library model is recorded as
  provenance only. The source host's device node is deliberately omitted — it is runtime
  state of the writing machine and is meaningless on the (different) recovery hardware.
- **age private identity** — the decryption secret (see below).
- **Recovery procedure** — the human-readable, step-by-step recovery text.

## Write health

For every physical tape written on real hardware, the report records an **observational**
write-health measurement taken after the tape's write window closes (unmount and the
deferred LTFS index sync have settled). It exists to evaluate the anti-shoe-shining rate
(project principle 2, SPEC §2/§14) on every run against the real workload. It is purely
observational — **it never affects run success**: a tape flagged here was still written
successfully.

Each tape row records:

- **Throughput (MB/s)** — sustained write throughput over the write window, computed as
  the tape's staged size (the archive data, in decimal MB) divided by the write-window
  elapsed time.
- **Floor** — the speed-matching floor the throughput is compared against. It is a
  property of the **tape generation being written** (the write format determines the
  drive's speed-matching range), derived from the configured native capacity
  (`library.tapeCapacityBytes`, SPEC §5/§14): LTO-5 40, LTO-6 50, LTO-7 100, LTO-8 112,
  LTO-9 180 MB/s (nominal published minimum speed-matching rates; slightly
  vendor-dependent). A capacity that maps to no known generation renders the floor as
  `n/a` — the throughput is still reported, but no below-floor verdict is made rather
  than judging against a guessed number.
- **Repositions** — the drive's back-hitch count from SCSI log page `0x24`. A drive that
  does not support the page reports zero.
- **Status** — `healthy` when the tape streamed at or above a known floor with zero
  repositions and no TapeAlert flags; otherwise the specific flags: `below floor`,
  `N repositions`, the active `TapeAlert` flag descriptions from log page `0x2e`, and/or
  `floor unknown for this LTO generation` when no floor is recorded for the generation.

A tape that carries no measurement (e.g. a virtual/dry-run tape, which does not reflect
real throughput) renders as `not measured`.

The same per-tape values are also exported as Prometheus gauges on the data worker's
`/metrics` endpoint, labelled by `barcode`: `tape_archiver_write_throughput_mbps`,
`tape_archiver_write_repositions`, `tape_archiver_write_tapealert_flags`, and
`tape_archiver_write_below_floor`.

## The age private identity is included on purpose

The report **intentionally** contains the age private identity
(`AGE-SECRET-KEY-PQ-1…`). This is the documented key-escrow decision (SPEC §7): whoever
holds the printed report — or the recovery ISO that embeds it — can always decrypt the
archives, with no dependency on an external key store ~20 years later.

The consequence, stated plainly: the report (its Discord delivery, and the recovery ISO
on its burned disc) contains the decryption secret and **must be handled accordingly**. Under this
project's personal cold-storage threat model that trade-off is accepted. A finding of the
private identity in the report is expected behavior, not a leak.

The identity comes from the run config (`encryption.identity`) and is **never used to
encrypt** — encryption uses `encryption.recipients` only. Before embedding it, the
Report phase derives its public key and confirms it is one of the configured recipients,
failing the run otherwise; the report can never escrow a key that cannot decrypt the
archives.

## Implementation notes

- Rendering uses the pure-Go [`github.com/go-pdf/fpdf`](https://github.com/go-pdf/fpdf)
  library. Pure Go keeps the build hermetic and avoids a runtime tool dependency, in
  line with the 20-year-recoverability principle (SPEC §2).
- Field presence is verified in tests by extracting the rendered PDF's text with the
  pure-Go [`github.com/ledongthuc/pdf`](https://github.com/ledongthuc/pdf) reader, so the
  test reads back the real rendered content rather than trusting the writer.
