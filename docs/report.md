# Run Report (PDF)

Every run produces a single PDF report (SPEC §9): the durable, printable, laminated
**offline index** for that run. It is built by `pkg/report` from a complete run manifest
and is intended to be printed and stored alongside the physical tapes, and is also
embedded in the recovery ISO.

## Contents

The report contains, at minimum:

- **Run** — run id and date.
- **Contents manifest** — every archive, with its member volumes, source snapshots, and
  per-file sizes and SHA-256 checksums.
- **Tapes** — which physical tape (by barcode/label) holds what.
- **Build parameters** — how the tapes were built: tool version, `age`/`par2`/`ltfs`
  versions, slice size, PAR2 redundancy, and the drive/library identifiers. The drive is
  recorded as its model, the **LTO generation required to read the tape** (the fact a
  future recoverer actually needs), and its serial; the library model is recorded as
  provenance only. The source host's device node is deliberately omitted — it is runtime
  state of the writing machine and is meaningless on the (different) recovery hardware.
- **age private identity** — the decryption secret (see below).
- **Recovery procedure** — the human-readable, step-by-step recovery text.

## The age private identity is included on purpose

The report **intentionally** contains the age private identity
(`AGE-SECRET-KEY-PQ-1…`). This is the documented key-escrow decision (SPEC §7): whoever
holds the printed report — or the recovery ISO that embeds it — can always decrypt the
archives, with no dependency on an external key store ~20 years later.

The consequence, stated plainly: the report (and the ISO and the Discord delivery that
carry it) contain the decryption secret and **must be handled accordingly**. Under this
project's personal cold-storage threat model that trade-off is accepted. A finding of the
private identity in the report is expected behavior, not a leak.

## Implementation notes

- Rendering uses the pure-Go [`github.com/go-pdf/fpdf`](https://github.com/go-pdf/fpdf)
  library. Pure Go keeps the build hermetic and avoids a runtime tool dependency, in
  line with the 20-year-recoverability principle (SPEC §2).
- Field presence is verified in tests by extracting the rendered PDF's text with the
  pure-Go [`github.com/ledongthuc/pdf`](https://github.com/ledongthuc/pdf) reader, so the
  test reads back the real rendered content rather than trusting the writer.
