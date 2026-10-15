package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// PAR2 accepts only whole redundancy percentages in [1, 100].
const (
	minRedundancyPercent = 1
	maxRedundancyPercent = 100
)

// validateRedundancyPercent rejects fractional values too: PAR2 honors only
// integers, so a fraction would make the feasibility pre-check and Pack disagree.
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

	// Copies is not bounded by the drive count: copies are written in successive
	// drive-sets. Blank slots must form whole copy sets, since leftovers could
	// never complete another logical tape's copies.
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

	// Reject blank recipients up front rather than hours later at encryption;
	// key syntax is checked in pkg/agewrap.
	for i, recipient := range c.Encryption.Recipients {
		if strings.TrimSpace(recipient) == "" {
			return fmt.Errorf("encryption.recipients[%d]: must not be empty", i)
		}
	}

	// Required up front so a run doesn't fail at Report after hours of writing.
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
	if o == nil {
		return nil
	}

	// A present block that burns nothing would let a run succeed without the
	// recovery disc the operator asked for; omitting the block disables burning.
	if len(o.Drives) == 0 {
		return errors.New("delivery.opticalBurn.drives: at least one burner drive is required (omit opticalBurn to disable burning)")
	}

	if o.Copies < 1 {
		return fmt.Errorf("delivery.opticalBurn.copies: must be >= 1 (omit opticalBurn to disable burning), got %d", o.Copies)
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
	// Other characters are sanitized at use; only a blank label is an error.
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

	// An empty namespace is valid only with a labelSelector (cluster-wide).
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

	// planDriveSets consumes BlankSlots positionally with no dedup, so a duplicate
	// would otherwise fail only at Load, after hours of staging. Zero is legal.
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

	return nil
}
