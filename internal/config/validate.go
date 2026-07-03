package config

import (
	"fmt"
	"strings"
)

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

	if err := c.Redundancy.validate(); err != nil {
		return err
	}

	if len(c.Encryption.Recipients) == 0 {
		return fmt.Errorf("encryption.recipients: at least one recipient is required")
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

	return nil
}

func (s Source) validate(index int) error {
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

	if k.Namespace == "" {
		return fmt.Errorf("sources[%d].k8s.namespace: must not be empty", index)
	}

	hasName := k.Name != ""
	hasSelector := k.LabelSelector != ""

	switch {
	case !hasName && !hasSelector:
		return fmt.Errorf("sources[%d].k8s: one of name or labelSelector must be set", index)
	case hasName && hasSelector:
		return fmt.Errorf("sources[%d].k8s: name and labelSelector are mutually exclusive", index)
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

	if len(l.BlankSlots) == 0 {
		return fmt.Errorf("library.blankSlots: at least one blank slot is required")
	}

	if l.TapeCapacityBytes <= 0 {
		return fmt.Errorf("library.tapeCapacityBytes: must be > 0, got %d", l.TapeCapacityBytes)
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
	case hasPercent && *r.TargetPercentage < 0:
		return fmt.Errorf("redundancy.targetPercentage: must be >= 0, got %v", *r.TargetPercentage)
	case hasFill && r.FillToCapacity.Floor < 0:
		return fmt.Errorf("redundancy.fillToCapacity.floor: must be >= 0, got %v", r.FillToCapacity.Floor)
	}

	if r.SliceSizeBytes <= 0 {
		return fmt.Errorf("redundancy.sliceSizeBytes: must be > 0, got %d", r.SliceSizeBytes)
	}

	return nil
}
