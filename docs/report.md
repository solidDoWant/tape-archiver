# Run Report (PDF)

Every run produces a single PDF report (SPEC §9): the durable, printable, laminated
**offline index** for that run. It is built by `pkg/report` from a complete run manifest
and is intended to be printed and stored alongside the physical tapes, and is also
embedded in the recovery ISO.

A dry-run report carries a red banner under the title and a "DRY RUN — no real tapes were written" footer on every page: its barcodes are the real library's, mirrored onto the virtual one, and no real tape holds its data.

## Contents

The report contains, at minimum:

- **Run** — the run id and date. The run id is the run's own identifier — the one
  `tapectl` and the web UI show for that run — not the fixed `backup` workflow id every
  run shares, so a printed report always names the single run it describes.
- **Contents manifest** — every archive, with its on-tape directory
  (`archives/NNN-<label>/`), member volumes, source snapshots, whether the archive is
  compressed, and per-file sizes and SHA-256 checksums. Compression is configured per
  source ([`sources[].compression`](configuration.md)), so it is recorded per archive:
  it tells a recoverer whether a `zstd` step stands between the decrypted stream and the
  tar, rather than leaving them to identify the format from its magic bytes.
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
  versions, slice size, PAR2 redundancy, and the tape/drive/library identifiers. The
  **tape generation needed to read the tape** is the generation of the media themselves
  (e.g. `LTO-5`), read from each cartridge's SCSI density code during its write window —
  the only point at which the medium is loaded, since the Report phase runs after the
  tapes are unloaded. That is the fact a future recoverer actually needs, and it is
  deliberately not the writing drive's generation: an LTO-6 drive writes LTO-5 media, and
  LTO read compatibility is bounded (an LTO-8 drive reads neither LTO-6 nor LTO-5), so
  naming the drive would send a recoverer after hardware that cannot read these tapes. It
  renders `mixed` when a run's tapes are not all one generation and `unknown` when the
  medium could not be read. The drive's model, generation, and serial follow as
  provenance — what wrote the tapes — as does the library model. The source host's device
  node is deliberately omitted; it is runtime state of the writing machine and is
  meaningless on the (different) recovery hardware.
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

- **Throughput (MB/s)** — sustained write throughput while streaming, computed as the
  total bytes written to the tape (archive slices + PAR2 recovery files, in decimal MB)
  divided by the time spent copying the staged tree to the mounted LTFS volume. The
  LTFS mount, the per-tape manifest, and the unmount-time index write are excluded:
  they are a fixed overhead (about a minute on LTO-5) that says nothing about whether
  the drive streamed. Both slices and PAR2 are copied inside the timed copy, so counting
  every written byte matches the numerator to the denominator. A tape that received
  less than 1 GB renders `not measurable` instead of a rate, with the status note
  `throughput not measurable (too little data)`: LTFS's write cache and the drive's
  buffer absorb a write that small before the tape catches up, so its rate cannot
  show streaming. No below-floor verdict is made for it, and it is not marked
  unhealthy for throughput alone (repositions and TapeAlert flags are still judged).
- **Floor** — the speed-matching floor the throughput is compared against. It is a
  property of the **tape generation being written** (the write format determines the
  drive's speed-matching range), derived from the configured native capacity
  (`library.tapeCapacityBytes`, SPEC §5/§14): LTO-5 40, LTO-6 50, LTO-7 100, LTO-8 112,
  LTO-9 180 MB/s (nominal published minimum speed-matching rates; slightly
  vendor-dependent). A capacity that maps to no known generation renders the floor as
  `n/a` — the throughput is still reported, but no below-floor verdict is made rather
  than judging against a guessed number.
- **Repositions** — the drive's back-hitch count (`total_suspended_writes`) from the SCSI
  Tape usage log page `0x30` (LTO-5/6). A drive that does not support the page renders
  `n/m` (not measured) rather than a zero, so an unread counter is never mistaken for a
  measured-zero clean write.
- **Status** — `healthy` when the tape streamed at or above a known floor with a measured
  reposition count of zero and no TapeAlert flags; otherwise the specific flags:
  `below floor`, `N repositions`, `reposition counter not supported` when the drive does
  not support page `0x30`, the active `TapeAlert` flag descriptions from log page `0x2e`,
  and/or `floor unknown for this LTO generation` when no floor is recorded for the
  generation.

A tape that carries no measurement (e.g. a virtual/dry-run tape, which does not reflect
real throughput) renders as `not measured`.

The same per-tape values are also exported as Prometheus gauges on the data worker's
`/metrics` endpoint, labelled by `barcode` and `dry_run` (`true`/`false`):
`tape_archiver_write_throughput_mbps`,
`tape_archiver_write_repositions`, `tape_archiver_write_tapealert_flags`, and
`tape_archiver_write_below_floor`. For a write too small to measure, no
`tape_archiver_write_throughput_mbps` sample is exported for that barcode (any earlier
one is removed) and `tape_archiver_write_below_floor` is `0`. A dry run mirrors the real
library, so its virtual tapes carry real barcodes: alert on `dry_run="false"`, so a
rehearsal never fires on, or overwrites, a real tape's reading.

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
