package config

// DefaultFeasibilityOverhead is the multiplier applied to a source's
// logicalreferenced size in the Resolve feasibility pre-check (SPEC.md §4.3
// phase 1, §16) when Config.FeasibilityOverhead is unset. It covers tar/zstd/age
// framing: age STREAM framing adds ~0.02% and tar headers/padding are typically
// well under 1%, while zstd is assumed to give no reduction (the incompressible
// worst case) so the estimate never runs low. 5% is a deliberately generous
// margin for many-small-file datasets; it tunes only the cheap pre-check, never
// the authoritative measured size produced by Prepare.
const DefaultFeasibilityOverhead = 1.05

// Config fully describes a backup run. It is the single source of truth for a run.
type Config struct {
	Sources             []Source   `json:"sources"`
	Copies              int        `json:"copies"`
	Library             Library    `json:"library"`
	Redundancy          Redundancy `json:"redundancy"`
	Encryption          Encryption `json:"encryption"`
	Delivery            Delivery   `json:"delivery"`
	FeasibilityOverhead *float64   `json:"feasibilityOverhead,omitempty"`
}

// EffectiveFeasibilityOverhead returns the configured FeasibilityOverhead
// multiplier, or DefaultFeasibilityOverhead when it is unset.
func (c *Config) EffectiveFeasibilityOverhead() float64 {
	if c.FeasibilityOverhead != nil {
		return *c.FeasibilityOverhead
	}

	return DefaultFeasibilityOverhead
}

// Source is a single item to archive. Exactly one of K8s or ZFSPath must be set.
// Compression defaults to enabled when nil.
type Source struct {
	Compression *bool          `json:"compression,omitempty"`
	K8s         *K8sRef        `json:"k8s,omitempty"`
	ZFSPath     *ZFSPathSource `json:"zfsPath,omitempty"`
}

// K8sRef identifies a Kubernetes snapshot resource by GVK, namespace, and name or
// label selector. APIVersion and Kind follow standard k8s manifest syntax, e.g.
// "snapshot.storage.k8s.io/v1" + "VolumeSnapshot" or
// "groupsnapshot.storage.k8s.io/v1alpha1" + "VolumeGroupSnapshot".
// Exactly one of Name or LabelSelector must be set.
type K8sRef struct {
	APIVersion    string `json:"apiVersion"`
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name,omitempty"`
	LabelSelector string `json:"labelSelector,omitempty"`
}

// ZFSPathSource is an explicit ZFS snapshot or dataset on the pool.
type ZFSPathSource struct {
	Name string `json:"name"`
}

// Library specifies the tape library hardware and the blank tapes to use.
type Library struct {
	Changer    string   `json:"changer"`
	Drives     []string `json:"drives"`
	BlankSlots []int    `json:"blankSlots"`
	// TapeCapacityBytes is the native (uncompressed) capacity of one tape, in
	// bytes (e.g. 2_500_000_000_000 for LTO-6). It is the single-tape ceiling the
	// Resolve feasibility pre-check tests an archive's estimate against and the
	// capacity the Pack phase bin-packs into; runs plan against native capacity
	// with LTO hardware compression disabled (SPEC §4.3).
	TapeCapacityBytes int64 `json:"tapeCapacityBytes"`
}

// Redundancy specifies the PAR2 redundancy policy.
// Exactly one of TargetPercentage or FillToCapacity must be set.
type Redundancy struct {
	TargetPercentage *float64    `json:"targetPercentage,omitempty"`
	FillToCapacity   *FillConfig `json:"fillToCapacity,omitempty"`
	SliceSizeBytes   int64       `json:"sliceSizeBytes"`
}

// FillConfig configures fill-to-capacity PAR2 mode, which expands redundancy to
// consume remaining tape space down to a minimum floor percentage.
type FillConfig struct {
	Floor float64 `json:"floor"`
}

// Encryption specifies the age recipients to encrypt archives to.
type Encryption struct {
	Recipients []string `json:"recipients"`
}

// Delivery specifies how run artifacts are delivered on success.
type Delivery struct {
	WebhookURL string `json:"webhookUrl"`
}
