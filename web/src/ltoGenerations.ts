// ltoGenerations.ts is the config page's tape-capacity generation table
// (Library.tsx's "tape capacity" <select>, DESIGN_ANALYSIS.md §2 "D. Config").
//
// Values are chosen to fall unambiguously into the matching generation
// bucket workflows/backup/report.go's own ltoGeneration classifier uses
// (LTO-6 >= 2 TB, LTO-7 >= 5 TB, LTO-8 >= 10 TB, LTO-9 >= 16 TB), and are the
// generations' real native capacities — not the design mock's, which quoted
// "LTO-6 · 2.4 TB" (a factual error the issue's technical context flagged:
// LTO-6 native capacity is 2.5 TB, matching docs/configuration.md's own
// library.tapeCapacityBytes example, 2500000000000).
export interface LtoGeneration {
  label: string
  capacityBytes: number
  capacityLabel: string
}

export const ltoGenerations: LtoGeneration[] = [
  { label: 'LTO-6', capacityBytes: 2_500_000_000_000, capacityLabel: '2.5 TB' },
  { label: 'LTO-7', capacityBytes: 6_000_000_000_000, capacityLabel: '6 TB' },
  { label: 'LTO-8', capacityBytes: 12_000_000_000_000, capacityLabel: '12 TB' },
  { label: 'LTO-9', capacityBytes: 18_000_000_000_000, capacityLabel: '18 TB' },
]

export const defaultLtoGeneration = ltoGenerations[0]

// ltoGenerationForCapacity finds the table entry whose capacityBytes exactly
// matches bytes, for reconstructing a Form-mode selection from a config
// built elsewhere (e.g. switching from JSON mode into Form mode). Returns
// undefined for a capacity this table has no exact entry for — the caller
// decides the fallback (configModel.ts's configToFormState falls back to
// defaultLtoGeneration, since Form mode's <select> can only ever choose one
// of this fixed table's values, unlike JSON mode's free-form number).
export function ltoGenerationForCapacity(bytes: number): LtoGeneration | undefined {
  return ltoGenerations.find((generation) => generation.capacityBytes === bytes)
}
