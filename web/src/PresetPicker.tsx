import type { RunPreset } from './uiConfig'

export interface PresetPickerProps {
  presets: RunPreset[]
  // loaded is the name of the preset last loaded into the form, if any.
  loaded: string | null
  onLoad: (preset: RunPreset) => void
}

// PresetPicker offers the deployment's run presets (issue #417); loading one
// fills the form, which stays fully editable before submit.
function PresetPicker({ presets, loaded, onLoad }: PresetPickerProps) {
  if (presets.length === 0) {
    return null
  }

  return (
    <section aria-label="Presets" className="rounded-xl border border-border bg-surface p-4.5 shadow-card sm:p-5">
      <h2 className="mb-1 text-[13px] font-semibold text-text">Presets</h2>
      <p className="mb-3.5 text-[11.5px] text-text-dim">
        Start from a preset supplied by this deployment. Loading one replaces the form; you can still edit every field.
      </p>
      <ul className="flex flex-col gap-2">
        {presets.map((preset) => (
          <li
            key={preset.name}
            className="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-2 px-3.5 py-2.5"
          >
            <div className="min-w-0 flex-1">
              <div className="font-mono text-[12px] font-semibold text-text">{preset.name}</div>
              {preset.description ? <div className="mt-0.5 text-[11.5px] text-text-dim">{preset.description}</div> : null}
            </div>
            <button
              type="button"
              aria-label={`Load preset ${preset.name}`}
              onClick={() => onLoad(preset)}
              className="rounded-lg border border-border-strong bg-surface px-3.5 py-1.5 text-[12px] font-medium text-text transition-colors hover:bg-inset"
            >
              {loaded === preset.name ? 'Reload' : 'Load'}
            </button>
          </li>
        ))}
      </ul>
    </section>
  )
}

export default PresetPicker
