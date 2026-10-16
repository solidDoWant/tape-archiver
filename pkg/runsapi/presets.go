// Run presets (issue #417): deployment-supplied, named partial run configs the
// "Start new run" page loads into its form. They only pre-fill the form; the
// submitted config stays the run's single source of truth (SPEC §4.2).
package runsapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// Placeholders stand in for fields a partial preset leaves to the operator, so
// config.Validate can check everything the preset does set.
const (
	placeholderSource    = "preset-placeholder"
	placeholderRecipient = "age1presetplaceholder"
	placeholderIdentity  = "AGE-SECRET-KEY-PRESET-PLACEHOLDER"
	placeholderDevice    = "/dev/preset-placeholder"
	placeholderPercent   = 10
)

// Preset is one named partial run config, as the deployment supplies it.
type Preset struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Config      json.RawMessage `json:"config"`

	// decodeErr is a per-entry decode failure, reported when presets are validated.
	decodeErr error
}

// uiPreset is a validated preset as GET /api/config/ui returns it.
type uiPreset struct {
	Name        string         `json:"name" doc:"Unique preset name."`
	Description string         `json:"description" doc:"Operator-facing description; may be empty."`
	Config      map[string]any `json:"config" doc:"Partial run config (a subset of GET /api/config/schema's document) to pre-fill the form with."`
}

// LoadPresetsFile reads a JSON array of presets. An empty path yields none. A
// malformed entry is kept with its decode error so validation can report it by
// name without discarding the others.
func LoadPresetsFile(path string) ([]Preset, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read run presets file: %w", err)
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse run presets file %s: must be a JSON array of presets: %w", path, err)
	}

	presets := make([]Preset, 0, len(entries))

	for i, entry := range entries {
		decoder := json.NewDecoder(bytes.NewReader(entry))
		decoder.DisallowUnknownFields()

		var preset Preset
		if err := decoder.Decode(&preset); err != nil {
			// Salvage the name for the log line when the entry is otherwise malformed.
			var named struct {
				Name string `json:"name"`
			}

			_ = json.Unmarshal(entry, &named)

			preset = Preset{Name: named.Name, decodeErr: fmt.Errorf("presets[%d]: %w", i, err)}
		}

		presets = append(presets, preset)
	}

	return presets, nil
}

// WithPresets supplies the deployment's run presets. They are validated once
// the handler is built; invalid ones are logged and not offered.
func WithPresets(presets []Preset) Option {
	return func(h *handler) { h.rawPresets = presets }
}

// validatePresets keeps the presets that validate against this deployment,
// logging each rejected one with its name and error.
func (h *handler) validatePresets() []uiPreset {
	valid := make([]uiPreset, 0, len(h.rawPresets))
	seen := make(map[string]struct{}, len(h.rawPresets))

	for _, preset := range h.rawPresets {
		offered, err := h.validatePreset(preset, seen)
		if err != nil {
			slog.Warn("runsapi: run preset is invalid and will not be offered", "preset", preset.Name, "error", err)

			continue
		}

		seen[preset.Name] = struct{}{}

		valid = append(valid, offered)
	}

	if len(h.rawPresets) > 0 {
		slog.Info("runsapi: loaded run presets", "offered", len(valid), "rejected", len(h.rawPresets)-len(valid))
	}

	return valid
}

func (h *handler) validatePreset(preset Preset, seen map[string]struct{}) (uiPreset, error) {
	if preset.decodeErr != nil {
		return uiPreset{}, preset.decodeErr
	}

	if strings.TrimSpace(preset.Name) == "" {
		return uiPreset{}, errors.New("name: must not be empty")
	}

	if _, ok := seen[preset.Name]; ok {
		return uiPreset{}, fmt.Errorf("name: duplicate preset name %q", preset.Name)
	}

	// UseNumber keeps large integers (tapeCapacityBytes) exact when re-served.
	var tree map[string]any

	treeDecoder := json.NewDecoder(bytes.NewReader(preset.Config))
	treeDecoder.UseNumber()

	if err := treeDecoder.Decode(&tree); err != nil || tree == nil {
		return uiPreset{}, errors.New("config: must be a JSON object (a partial run config)")
	}

	// The same strict decode as POST /api/runs: unknown fields and wrong types fail.
	decoder := json.NewDecoder(bytes.NewReader(preset.Config))
	decoder.DisallowUnknownFields()

	var cfg config.Config
	if err := decoder.Decode(&cfg); err != nil {
		return uiPreset{}, fmt.Errorf("config: %w", err)
	}

	// Presets are served to every browser, so they must not carry credentials.
	if cfg.Encryption.Identity != "" {
		return uiPreset{}, errors.New("config.encryption.identity: a preset must not carry the age private identity; enter it per run")
	}

	if cfg.Delivery.WebhookURL != "" {
		return uiPreset{}, errors.New("config.delivery.webhookUrl: a preset must not carry the webhook URL (a credential); set it in the deployment")
	}

	fillPresetPlaceholders(&cfg, tree)
	h.applyDeployConfig(&cfg)
	fillDeployPlaceholders(&cfg)

	if err := cfg.Validate(); err != nil {
		return uiPreset{}, fmt.Errorf("config (merged with deploy-owned fields): %w", err)
	}

	if hasPath(tree, "library", "blankSlots") {
		if err := h.validateBlankSlotsAgainstTopology(&cfg); err != nil {
			return uiPreset{}, fmt.Errorf("config: %w", err)
		}
	}

	return uiPreset{Name: preset.Name, Description: preset.Description, Config: tree}, nil
}

// fillPresetPlaceholders fills only fields absent from the preset, so any value
// the preset does set is validated as written.
func fillPresetPlaceholders(cfg *config.Config, tree map[string]any) {
	if !hasPath(tree, "sources") {
		cfg.Sources = []config.Source{{ZFSPath: &config.ZFSPathSource{Name: placeholderSource}}}
	}

	if !hasPath(tree, "copies") {
		cfg.Copies = 1
	}

	if !hasPath(tree, "library", "blankSlots") {
		// Whole copy sets, so the blank-slots-multiple-of-copies rule holds.
		count := max(cfg.Copies, 1)

		cfg.Library.BlankSlots = make([]int, count)
		for i := range cfg.Library.BlankSlots {
			cfg.Library.BlankSlots[i] = i + 1
		}
	}

	if !hasPath(tree, "library", "tapeCapacityBytes") {
		cfg.Library.TapeCapacityBytes = 1
	}

	if !hasPath(tree, "redundancy", "targetPercentage") && !hasPath(tree, "redundancy", "fillToCapacity") {
		percent := float64(placeholderPercent)
		cfg.Redundancy.TargetPercentage = &percent
	}

	if !hasPath(tree, "encryption", "recipients") {
		cfg.Encryption.Recipients = []string{placeholderRecipient}
	}

	cfg.Encryption.Identity = placeholderIdentity
}

// fillDeployPlaceholders fills deploy-owned devices this deployment leaves
// unset; the form shows them as "not configured" rather than the preset owning them.
func fillDeployPlaceholders(cfg *config.Config) {
	if cfg.Library.Changer == "" {
		cfg.Library.Changer = placeholderDevice
	}

	if len(cfg.Library.Drives) == 0 {
		cfg.Library.Drives = []string{placeholderDevice}
	}

	if cfg.Delivery.OpticalBurn != nil && len(cfg.Delivery.OpticalBurn.Drives) == 0 {
		cfg.Delivery.OpticalBurn.Drives = []string{placeholderDevice}
	}
}

// hasPath reports whether the nested object key path is present in tree. Keys
// match case-insensitively, as encoding/json decodes them, so a placeholder
// never overwrites a field the preset sets under different casing.
func hasPath(tree map[string]any, keys ...string) bool {
	current := any(tree)

	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}

		current, ok = lookupFold(object, key)
		if !ok {
			return false
		}
	}

	return true
}

func lookupFold(object map[string]any, key string) (any, bool) {
	if value, ok := object[key]; ok {
		return value, true
	}

	for name, value := range object {
		if strings.EqualFold(name, key) {
			return value, true
		}
	}

	return nil, false
}
