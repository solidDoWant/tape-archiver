package config

import "time"

// DefaultIOWaitTimeout is how long the Eject phase waits for an operator to clear
// the import/export station when it fills, before the run fails (SPEC §4.3 phase
// 8), when Library.IOWaitTimeoutSeconds is unset. It bounds the operator-in-the-
// loop pause so an unattended run always reaches a defined end state rather than
// waiting indefinitely.
const DefaultIOWaitTimeout = 12 * time.Hour

// DefaultWriteFailureWaitTimeout is how long the tape path waits for the operator
// to resume or abort a run paused because a Load or Write failed for one drive-set
// (SPEC §4.3), when Library.WriteFailureWaitTimeoutSeconds is unset. Like
// DefaultIOWaitTimeout it bounds the operator-in-the-loop pause so an unattended
// run always reaches a defined end state rather than waiting indefinitely.
const DefaultWriteFailureWaitTimeout = 12 * time.Hour

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
	// IOWaitTimeoutSeconds bounds how long the Eject phase waits for the operator
	// to clear the import/export station when it fills before more tapes can be
	// exported (SPEC §4.3 phase 8). When unset, DefaultIOWaitTimeout applies. It
	// must be positive when set: the wait is always bounded so an unattended run
	// reaches a defined end state (every written tape in an I/O or storage slot,
	// none in a drive) rather than pausing forever.
	IOWaitTimeoutSeconds *int `json:"ioWaitTimeoutSeconds,omitempty"`
	// WriteFailureWaitTimeoutSeconds bounds how long the tape path waits for the
	// operator to resume or abort a run paused because a Load or Write failed for
	// one drive-set (SPEC §4.3). When unset, DefaultWriteFailureWaitTimeout applies.
	// It must be positive when set: the wait is always bounded so an unattended run
	// reaches a defined end state (every tape in a drive, I/O, or storage slot)
	// rather than pausing forever.
	WriteFailureWaitTimeoutSeconds *int `json:"writeFailureWaitTimeoutSeconds,omitempty"`
}

// EffectiveIOWaitTimeout returns the configured operator I/O-station wait, or
// DefaultIOWaitTimeout when Library.IOWaitTimeoutSeconds is unset.
func (l Library) EffectiveIOWaitTimeout() time.Duration {
	if l.IOWaitTimeoutSeconds != nil {
		return time.Duration(*l.IOWaitTimeoutSeconds) * time.Second
	}

	return DefaultIOWaitTimeout
}

// EffectiveWriteFailureWaitTimeout returns the configured operator wait for a
// Load/Write-failure pause, or DefaultWriteFailureWaitTimeout when
// Library.WriteFailureWaitTimeoutSeconds is unset.
func (l Library) EffectiveWriteFailureWaitTimeout() time.Duration {
	if l.WriteFailureWaitTimeoutSeconds != nil {
		return time.Duration(*l.WriteFailureWaitTimeoutSeconds) * time.Second
	}

	return DefaultWriteFailureWaitTimeout
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

// Encryption specifies the age recipients to encrypt archives to, and the
// matching private identity escrowed into the run's report and recovery ISO.
type Encryption struct {
	Recipients []string `json:"recipients"`
	// Identity is the age private identity (AGE-SECRET-KEY-PQ-1…) escrowed into
	// the PDF report and recovery ISO so the holder of those artifacts can always
	// decrypt (SPEC §7 key-escrow decision). It is NEVER used to encrypt —
	// encryption uses Recipients only — so archives are protected regardless of
	// where this identity is held. The Report phase verifies its derived public
	// recipient is among Recipients before embedding it. Because the report and
	// ISO therefore carry the decryption secret, deliver and store them
	// accordingly. Required.
	Identity string `json:"identity"`
}

// Delivery specifies how run artifacts are delivered on success.
type Delivery struct {
	WebhookURL string `json:"webhookUrl"`
}
