package config

import (
	"time"

	"github.com/invopop/jsonschema"
)

// DefaultIOWaitTimeout bounds how long Eject waits for an operator to clear a full
// import/export station when Library.IOWaitTimeoutSeconds is unset.
const DefaultIOWaitTimeout = 12 * time.Hour

// DefaultWriteFailureWaitTimeout bounds how long a run paused on a Load/Write
// failure waits for resume/abort when Library.WriteFailureWaitTimeoutSeconds is unset.
const DefaultWriteFailureWaitTimeout = 12 * time.Hour

// DefaultBurnWaitTimeout bounds how long a run paused on an optical burn failure
// waits for the operator when OpticalBurn.BurnWaitTimeoutSeconds is unset.
const DefaultBurnWaitTimeout = 12 * time.Hour

// DefaultFeasibilityOverhead is the multiplier on a source's logicalreferenced
// size in the Resolve feasibility pre-check when Config.FeasibilityOverhead is
// unset. It covers tar/age framing and assumes zstd gives no reduction, so the
// estimate never runs low; it never affects the measured size from Prepare.
const DefaultFeasibilityOverhead = 1.05

// Config fully describes a backup run. It is the single source of truth for a run.
type Config struct {
	Sources             []Source   `json:"sources" jsonschema:"minItems=1"`
	Copies              int        `json:"copies" jsonschema:"minimum=1"`
	Library             Library    `json:"library"`
	Redundancy          Redundancy `json:"redundancy"`
	Encryption          Encryption `json:"encryption"`
	Delivery            Delivery   `json:"delivery"`
	FeasibilityOverhead *float64   `json:"feasibilityOverhead,omitempty"`
}

// EffectiveFeasibilityOverhead returns FeasibilityOverhead, or
// DefaultFeasibilityOverhead when unset.
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
	// Label overrides the on-tape archive directory name (archives/NNN-label/). It
	// is sanitized and length-bounded; uniqueness comes from the NNN prefix.
	Label *string `json:"label,omitempty"`
}

// K8sRef identifies a Kubernetes snapshot resource by GVK, namespace, and either
// Name or LabelSelector (exactly one must be set).
type K8sRef struct {
	APIVersion string `json:"apiVersion" jsonschema:"minLength=1"`
	Kind       string `json:"kind" jsonschema:"minLength=1"`
	Namespace  string `json:"namespace"`
	// minLength=1 rejects a present-but-empty name/selector, which would otherwise
	// pass schema validation.
	Name          string `json:"name,omitempty" jsonschema:"minLength=1"`
	LabelSelector string `json:"labelSelector,omitempty" jsonschema:"minLength=1"`
}

// JSONSchemaExtend requires namespace only for a named snapshot; an empty
// namespace with a labelSelector selects cluster-wide.
func (K8sRef) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.Required = []string{"apiVersion", "kind"}
	schema.If = &jsonschema.Schema{Required: []string{"name"}}
	schema.Then = &jsonschema.Schema{Required: []string{"namespace"}}
}

// ZFSPathSource is an explicit ZFS snapshot or dataset on the pool.
type ZFSPathSource struct {
	Name string `json:"name" jsonschema:"minLength=1"`
}

// Library specifies the tape library hardware and the blank tapes to use.
type Library struct {
	Changer    string   `json:"changer" jsonschema:"minLength=1"`
	Drives     []string `json:"drives" jsonschema:"minItems=1"`
	BlankSlots []int    `json:"blankSlots" jsonschema:"minItems=1"`
	// TapeCapacityBytes is the native (uncompressed) capacity of one tape; runs plan
	// against native capacity with hardware compression disabled.
	TapeCapacityBytes int64 `json:"tapeCapacityBytes" jsonschema:"minimum=1"`
	// IOWaitTimeoutSeconds overrides DefaultIOWaitTimeout. Must be positive when set
	// so an unattended run always reaches a defined end state.
	IOWaitTimeoutSeconds *int `json:"ioWaitTimeoutSeconds,omitempty"`
	// WriteFailureWaitTimeoutSeconds overrides DefaultWriteFailureWaitTimeout. Must
	// be positive when set.
	WriteFailureWaitTimeoutSeconds *int `json:"writeFailureWaitTimeoutSeconds,omitempty"`
	// AllowNonBlankTapes lets a run overwrite a non-blank tape (with a warning
	// recorded in the report) instead of failing before any write.
	AllowNonBlankTapes bool `json:"allowNonBlankTapes,omitempty"`
	// MirrorSourceChanger is the real changer whose slot inventory the mhvtl library
	// in Changer mirrors before the tape path, so a dry run meets the real run's
	// conditions. Set only on dry runs by runsubmit.ApplyDryRun. It authorizes no
	// destructive SCSI: only mhvtl's own state is rewritten. Cleared on real runs
	// at submission and ignored by the workflow unless the run is a dry run.
	MirrorSourceChanger string `json:"mirrorSourceChanger,omitempty"`
}

// EffectiveIOWaitTimeout returns IOWaitTimeoutSeconds, or DefaultIOWaitTimeout
// when unset.
func (l Library) EffectiveIOWaitTimeout() time.Duration {
	if l.IOWaitTimeoutSeconds != nil {
		return time.Duration(*l.IOWaitTimeoutSeconds) * time.Second
	}

	return DefaultIOWaitTimeout
}

// EffectiveWriteFailureWaitTimeout returns WriteFailureWaitTimeoutSeconds, or
// DefaultWriteFailureWaitTimeout when unset.
func (l Library) EffectiveWriteFailureWaitTimeout() time.Duration {
	if l.WriteFailureWaitTimeoutSeconds != nil {
		return time.Duration(*l.WriteFailureWaitTimeoutSeconds) * time.Second
	}

	return DefaultWriteFailureWaitTimeout
}

// Redundancy specifies the PAR2 redundancy policy. Exactly one of
// TargetPercentage or FillToCapacity must be set; the slice size is derived at
// Resolve (issue #324).
type Redundancy struct {
	TargetPercentage *float64    `json:"targetPercentage,omitempty" jsonschema:"minimum=1,maximum=100,multipleOf=1"`
	FillToCapacity   *FillConfig `json:"fillToCapacity,omitempty"`
}

// FillConfig configures fill-to-capacity PAR2 mode, which expands redundancy to
// consume remaining tape space down to a minimum floor percentage.
type FillConfig struct {
	Floor float64 `json:"floor" jsonschema:"minimum=1,maximum=100,multipleOf=1"`
}

// Encryption specifies the age recipients to encrypt archives to, and the
// matching private identity escrowed into the run's report and recovery ISO.
type Encryption struct {
	Recipients []string `json:"recipients" jsonschema:"minItems=1"`
	// Identity is the age private identity escrowed into the PDF report and
	// recovery ISO so those artifacts can always decrypt. It is never used to
	// encrypt; those artifacts therefore carry the decryption secret.
	Identity string `json:"identity" jsonschema:"minLength=1"`
}

// Delivery specifies how run artifacts are delivered on success.
type Delivery struct {
	WebhookURL string `json:"webhookUrl"`
	// OpticalBurn optionally burns the recovery disc to optical media; nil disables it.
	OpticalBurn *OpticalBurn `json:"opticalBurn,omitempty"`
}

// OpticalBurn configures optical recovery-disc burning, mirroring the tape
// Library fields.
type OpticalBurn struct {
	// Drives lists the optical burner device paths (e.g. /dev/sr0).
	Drives []string `json:"drives" jsonschema:"minItems=1"`
	// Copies is the number of discs to burn, in burn-sets of at most len(Drives).
	// Omit the whole block to disable burning.
	Copies int `json:"copies" jsonschema:"minimum=1"`
	// AllowNonBlankDiscs permits reclaiming rewritable media; write-once media can
	// never be overwritten regardless.
	AllowNonBlankDiscs bool `json:"allowNonBlankDiscs,omitempty"`
	// BurnWaitTimeoutSeconds overrides DefaultBurnWaitTimeout. Must be positive when set.
	BurnWaitTimeoutSeconds *int `json:"burnWaitTimeoutSeconds,omitempty"`
}

// Enabled reports whether optical burning is configured. It is nil-safe.
func (o *OpticalBurn) Enabled() bool {
	return o != nil && len(o.Drives) > 0 && o.Copies > 0
}

// EffectiveBurnWaitTimeout returns BurnWaitTimeoutSeconds, or
// DefaultBurnWaitTimeout when unset. It is nil-safe.
func (o *OpticalBurn) EffectiveBurnWaitTimeout() time.Duration {
	if o != nil && o.BurnWaitTimeoutSeconds != nil {
		return time.Duration(*o.BurnWaitTimeoutSeconds) * time.Second
	}

	return DefaultBurnWaitTimeout
}
