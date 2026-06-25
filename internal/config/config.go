package config

// Config fully describes a backup run. It is the single source of truth for a run.
type Config struct {
	Sources    []Source   `json:"sources"`
	Copies     int        `json:"copies"`
	Library    Library    `json:"library"`
	Redundancy Redundancy `json:"redundancy"`
	Encryption Encryption `json:"encryption"`
	Delivery   Delivery   `json:"delivery"`
}

// Source is a single item to archive. Exactly one of K8sSnapshot or ZFSPath must be set.
// Compression defaults to enabled when nil.
type Source struct {
	Compression *bool          `json:"compression,omitempty"`
	K8sSnapshot *K8sSnapshot   `json:"k8sSnapshot,omitempty"`
	ZFSPath     *ZFSPathSource `json:"zfsPath,omitempty"`
}

// K8sSnapshot references a VolumeSnapshot or snapshot group.
// Exactly one of (Name + Namespace) or LabelSelector must be set.
type K8sSnapshot struct {
	Name          string `json:"name,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	LabelSelector string `json:"labelSelector,omitempty"`
	// Group archives all matched snapshots as a single tar (one subdirectory per volume).
	Group bool `json:"group,omitempty"`
}

// ZFSPathSource is an explicit ZFS snapshot or dataset path on the pool.
type ZFSPathSource struct {
	Path string `json:"path"`
}

// Library specifies the tape library hardware and the blank tapes to use.
type Library struct {
	Changer    string   `json:"changer"`
	Drives     []string `json:"drives"`
	BlankSlots []int    `json:"blankSlots"`
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
