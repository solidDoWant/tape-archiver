package config

import (
	"fmt"
	"math"
	"strings"
)

// The PAR2 engine accepts only whole redundancy percentages in the inclusive
// range [1, 100] (SPEC §8). The config gate enforces that contract directly so
// out-of-range or fractional values are rejected up front rather than being
// silently clamped or rounded downstream.
const (
	minRedundancyPercent = 1
	maxRedundancyPercent = 100
)

// validateRedundancyPercent rejects a redundancy percentage that is not a whole
// number in [1, 100]. The value is a float because the config surface accepts
// JSON numbers, but the PAR2 engine only honors integers, so a fractional value
// would leave the feasibility pre-check and the Pack reservation disagreeing.
func validateRedundancyPercent(field string, value float64) error {
	if value < minRedundancyPercent || value > maxRedundancyPercent || value != math.Trunc(value) {
		return fmt.Errorf("%s: must be an integer in [%d, %d], got %v", field, minRedundancyPercent, maxRedundancyPercent, value)
	}

	return nil
}

// Validate checks all Config fields for correctness. It returns the first
// error found, with a message that names the offending field.
func (c *Config) Validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("sources: at least one source is required")
	}

	for i, source := range c.Sources {
		if err := source.validate(i); err != nil {
			return err
		}
	}

	if c.Copies < 1 {
		return fmt.Errorf("copies: must be at least 1, got %d", c.Copies)
	}

	if err := c.Library.validate(); err != nil {
		return err
	}

	// Copies is intentionally not bounded by the drive count: a run writes the
	// copies of each logical tape in successive drive-sets of at most len(Drives)
	// at a time (SPEC §4.3 phases 6–8). The tape path checks at run time that the
	// plan's physical tapes fit the configured drives and blank slots.

	// Every logical tape needs one blank per copy (physical tapes = logical tapes
	// × copies, SPEC §4.3), so the blank slots only form whole logical-tape sets
	// when their count is a positive multiple of Copies. A leftover (count mod
	// Copies != 0) is dead weight: those blanks can never complete another logical
	// tape's copy set. Reject it here rather than let the operator stage a run
	// whose slot selection can't be fully used. Copies >= 1 and len(BlankSlots)
	// >= 1 are already enforced above, so the multiple is guaranteed >= 1.
	if len(c.Library.BlankSlots)%c.Copies != 0 {
		return fmt.Errorf(
			"library.blankSlots: the number of blank slots (%d) must be a positive multiple of copies (%d), so every logical tape gets a full set of copies",
			len(c.Library.BlankSlots), c.Copies)
	}

	if err := c.Redundancy.validate(); err != nil {
		return err
	}

	if len(c.Encryption.Recipients) == 0 {
		return fmt.Errorf("encryption.recipients: at least one recipient is required")
	}

	// A blank/whitespace-only recipient element is rejected here so a run is not
	// staged and written for hours only to fail when the first archive is
	// encrypted in pkg/agewrap. Recipient syntax (the age1pq1 key format) stays
	// enforced at pkg/agewrap; this is a non-blank check only.
	for i, recipient := range c.Encryption.Recipients {
		if strings.TrimSpace(recipient) == "" {
			return fmt.Errorf("encryption.recipients[%d]: must not be empty", i)
		}
	}

	// The identity is escrowed into the report and ISO for every run (SPEC §7);
	// require it here so a run that would only fail at the Report phase — after
	// hours of staging and writing — is rejected up front. The Report phase
	// additionally verifies it matches a configured recipient.
	if strings.TrimSpace(c.Encryption.Identity) == "" {
		return fmt.Errorf("encryption.identity: the age private identity is required (escrowed into the report and recovery ISO, SPEC §7)")
	}

	if c.FeasibilityOverhead != nil && *c.FeasibilityOverhead < 1 {
		return fmt.Errorf("feasibilityOverhead: must be >= 1, got %v", *c.FeasibilityOverhead)
	}

	if err := c.Delivery.validate(); err != nil {
		return err
	}

	return nil
}

func (d Delivery) validate() error {
	return d.OpticalBurn.validate()
}

func (o *OpticalBurn) validate() error {
	// An absent section (or one that leaves burning disabled) is valid: burning is
	// off by default. Only the fields that are set are constrained.
	if o == nil {
		return nil
	}

	// Copies may be 0 (disabled) but never negative. It is intentionally not bounded
	// by the drive count: copies burn in successive burn-sets of at most len(Drives).
	if o.Copies < 0 {
		return fmt.Errorf("delivery.opticalBurn.copies: must be >= 0, got %d", o.Copies)
	}

	seen := make(map[string]struct{}, len(o.Drives))
	for i, drive := range o.Drives {
		if strings.TrimSpace(drive) == "" {
			return fmt.Errorf("delivery.opticalBurn.drives[%d]: must not be empty", i)
		}

		if _, ok := seen[drive]; ok {
			return fmt.Errorf("delivery.opticalBurn.drives[%d]: duplicate device path %q", i, drive)
		}

		seen[drive] = struct{}{}
	}

	if o.BurnWaitTimeoutSeconds != nil && *o.BurnWaitTimeoutSeconds <= 0 {
		return fmt.Errorf("delivery.opticalBurn.burnWaitTimeoutSeconds: must be > 0 when set, got %d", *o.BurnWaitTimeoutSeconds)
	}

	return nil
}

func (s Source) validate(index int) error {
	// A set-but-blank label is a mistake worth surfacing rather than silently
	// sanitizing to nothing; any other characters are sanitized at use, so no
	// further character validation is needed here.
	if s.Label != nil && strings.TrimSpace(*s.Label) == "" {
		return fmt.Errorf("sources[%d].label: must not be blank when set", index)
	}

	hasK8s := s.K8s != nil

	hasZFS := s.ZFSPath != nil
	switch {
	case !hasK8s && !hasZFS:
		return fmt.Errorf("sources[%d]: exactly one of k8s or zfsPath must be set", index)
	case hasK8s && hasZFS:
		return fmt.Errorf("sources[%d]: k8s and zfsPath are mutually exclusive", index)
	case hasK8s:
		return s.K8s.validate(index)
	default:
		if s.ZFSPath.Name == "" {
			return fmt.Errorf("sources[%d].zfsPath.name: must not be empty", index)
		}
	}

	return nil
}

func (k *K8sRef) validate(index int) error {
	if k.APIVersion == "" {
		return fmt.Errorf("sources[%d].k8s.apiVersion: must not be empty", index)
	}

	if k.Kind == "" {
		return fmt.Errorf("sources[%d].k8s.kind: must not be empty", index)
	}

	hasName := k.Name != ""
	hasSelector := k.LabelSelector != ""

	switch {
	case !hasName && !hasSelector:
		return fmt.Errorf("sources[%d].k8s: one of name or labelSelector must be set", index)
	case hasName && hasSelector:
		return fmt.Errorf("sources[%d].k8s: name and labelSelector are mutually exclusive", index)
	}

	// Namespace is required for a single named snapshot (a name has no cluster-wide
	// meaning), but optional for a labelSelector: an empty namespace there selects
	// across all namespaces (cluster-wide; SPEC §5, pkg/k8ssnap Ref).
	if hasName && k.Namespace == "" {
		return fmt.Errorf("sources[%d].k8s.namespace: must not be empty", index)
	}

	return nil
}

func (l Library) validate() error {
	if l.Changer == "" {
		return fmt.Errorf("library.changer: must not be empty")
	}

	if len(l.Drives) == 0 {
		return fmt.Errorf("library.drives: at least one drive is required")
	}

	seen := make(map[string]struct{}, len(l.Drives))
	for i, drive := range l.Drives {
		if strings.TrimSpace(drive) == "" {
			return fmt.Errorf("library.drives[%d]: must not be empty", i)
		}

		if _, ok := seen[drive]; ok {
			return fmt.Errorf("library.drives[%d]: duplicate device path %q", i, drive)
		}

		seen[drive] = struct{}{}
	}

	if len(l.BlankSlots) == 0 {
		return fmt.Errorf("library.blankSlots: at least one blank slot is required")
	}

	// A negative slot address can never name a real changer element, and a
	// repeated slot maps two logical tapes onto one physical slot: planDriveSets
	// consumes BlankSlots positionally with no dedup (workflows/backup/library.go),
	// so a duplicate only fails at Load, after hours of staging. Reject both here,
	// at the config gate, mirroring the duplicate-drive check above. Zero is a
	// legal element address and is allowed.
	seenSlots := make(map[int]struct{}, len(l.BlankSlots))
	for i, slot := range l.BlankSlots {
		if slot < 0 {
			return fmt.Errorf("library.blankSlots[%d]: must not be negative, got %d", i, slot)
		}

		if _, ok := seenSlots[slot]; ok {
			return fmt.Errorf("library.blankSlots[%d]: duplicate slot address %d", i, slot)
		}

		seenSlots[slot] = struct{}{}
	}

	if l.TapeCapacityBytes <= 0 {
		return fmt.Errorf("library.tapeCapacityBytes: must be > 0, got %d", l.TapeCapacityBytes)
	}

	if l.IOWaitTimeoutSeconds != nil && *l.IOWaitTimeoutSeconds <= 0 {
		return fmt.Errorf("library.ioWaitTimeoutSeconds: must be > 0 when set, got %d", *l.IOWaitTimeoutSeconds)
	}

	if l.WriteFailureWaitTimeoutSeconds != nil && *l.WriteFailureWaitTimeoutSeconds <= 0 {
		return fmt.Errorf("library.writeFailureWaitTimeoutSeconds: must be > 0 when set, got %d", *l.WriteFailureWaitTimeoutSeconds)
	}

	return nil
}

func (r Redundancy) validate() error {
	hasPercent := r.TargetPercentage != nil

	hasFill := r.FillToCapacity != nil
	switch {
	case !hasPercent && !hasFill:
		return fmt.Errorf("redundancy: one of targetPercentage or fillToCapacity must be set")
	case hasPercent && hasFill:
		return fmt.Errorf("redundancy: targetPercentage and fillToCapacity are mutually exclusive")
	case hasPercent:
		if err := validateRedundancyPercent("redundancy.targetPercentage", *r.TargetPercentage); err != nil {
			return err
		}
	case hasFill:
		if err := validateRedundancyPercent("redundancy.fillToCapacity.floor", r.FillToCapacity.Floor); err != nil {
			return err
		}
	}

	if r.SliceSizeBytes <= 0 {
		return fmt.Errorf("redundancy.sliceSizeBytes: must be > 0, got %d", r.SliceSizeBytes)
	}

	return nil
}
