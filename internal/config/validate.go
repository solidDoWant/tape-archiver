package config

import "fmt"

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

	if c.Copies > len(c.Library.Drives) {
		return fmt.Errorf("copies (%d) exceeds number of library.drives (%d)", c.Copies, len(c.Library.Drives))
	}

	if err := c.Redundancy.validate(); err != nil {
		return err
	}

	if len(c.Encryption.Recipients) == 0 {
		return fmt.Errorf("encryption.recipients: at least one recipient is required")
	}

	return nil
}

func (s Source) validate(index int) error {
	hasK8s := s.K8sSnapshot != nil

	hasZFS := s.ZFSPath != nil
	switch {
	case !hasK8s && !hasZFS:
		return fmt.Errorf("sources[%d]: exactly one of k8sSnapshot or zfsPath must be set", index)
	case hasK8s && hasZFS:
		return fmt.Errorf("sources[%d]: k8sSnapshot and zfsPath are mutually exclusive", index)
	case hasK8s:
		return s.K8sSnapshot.validate(index)
	default:
		if s.ZFSPath.Path == "" {
			return fmt.Errorf("sources[%d].zfsPath.path: must not be empty", index)
		}
	}

	return nil
}

func (k *K8sSnapshot) validate(index int) error {
	hasExplicit := k.Name != "" || k.Namespace != ""

	hasSelector := k.LabelSelector != ""
	switch {
	case !hasExplicit && !hasSelector:
		return fmt.Errorf("sources[%d].k8sSnapshot: one of (name/namespace) or labelSelector must be set", index)
	case hasExplicit && hasSelector:
		return fmt.Errorf("sources[%d].k8sSnapshot: (name/namespace) and labelSelector are mutually exclusive", index)
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
