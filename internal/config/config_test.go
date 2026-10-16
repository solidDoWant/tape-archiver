package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

func validConfig() Config {
	return Config{
		Sources: []Source{
			{ZFSPath: &ZFSPathSource{Name: "bulk-pool-01/archive@snap-20240101"}},
		},
		Copies: 2,
		Library: Library{
			Changer:           "/dev/sch0",
			Drives:            []string{"/dev/nst0", "/dev/nst1"},
			BlankSlots:        []int{1, 2},
			TapeCapacityBytes: 2_500_000_000_000,
		},
		Redundancy: Redundancy{
			TargetPercentage: ptr(10.0),
		},
		Encryption: Encryption{
			Recipients: []string{"age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"},
			Identity:   "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREALIDENTITY000000000000000000000000000000000",
		},
		Delivery: Delivery{
			WebhookURL: "https://discord.com/api/webhooks/123/abc",
		},
	}
}

func TestConfigRoundTrip(t *testing.T) {
	original := validConfig()
	original.Sources = append(original.Sources, Source{
		Compression: ptr(true),
		K8s: &K8sRef{
			APIVersion: "groupsnapshot.storage.k8s.io/v1alpha1",
			Kind:       "VolumeGroupSnapshot",
			Namespace:  "plex",
			Name:       "plex-group-snap",
		},
	})
	original.Sources = append(original.Sources, Source{
		K8s: &K8sRef{
			APIVersion:    "snapshot.storage.k8s.io/v1",
			Kind:          "VolumeSnapshot",
			Namespace:     "default",
			LabelSelector: "app=myapp",
		},
	})
	original.Redundancy = Redundancy{
		FillToCapacity: &FillConfig{Floor: 5.0},
	}
	original.Delivery.OpticalBurn = &OpticalBurn{
		Drives:                 []string{"/dev/sr0", "/dev/sr1"},
		Copies:                 2,
		AllowNonBlankDiscs:     true,
		BurnWaitTimeoutSeconds: ptr(3600),
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var roundTripped Config

	err = json.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, original, roundTripped)
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Config)
		wantErr     require.ErrorAssertionFunc
		errContains string
	}{
		{
			name:    "valid",
			mutate:  func(*Config) {},
			wantErr: require.NoError,
		},
		{
			name:        "no sources",
			mutate:      func(c *Config) { c.Sources = nil },
			wantErr:     require.Error,
			errContains: "sources",
		},
		{
			name:        "source with neither type",
			mutate:      func(c *Config) { c.Sources[0].ZFSPath = nil },
			wantErr:     require.Error,
			errContains: "sources[0]",
		},
		{
			name: "source with both types",
			mutate: func(c *Config) {
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshot",
					Namespace:  "ns",
					Name:       "s",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0]",
		},
		{
			name:        "zfsPath empty name",
			mutate:      func(c *Config) { c.Sources[0].ZFSPath.Name = "" },
			wantErr:     require.Error,
			errContains: "sources[0].zfsPath.name",
		},
		{
			name: "source label override is allowed",
			mutate: func(c *Config) {
				label := "cold-storage"
				c.Sources[0].Label = &label
			},
			wantErr: require.NoError,
		},
		{
			name: "source label set but blank",
			mutate: func(c *Config) {
				blank := "   "
				c.Sources[0].Label = &blank
			},
			wantErr:     require.Error,
			errContains: "sources[0].label",
		},
		{
			name: "k8s no apiVersion",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{Kind: "VolumeSnapshot", Namespace: "ns", Name: "s"}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s.apiVersion",
		},
		{
			name: "k8s no kind",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{APIVersion: "snapshot.storage.k8s.io/v1", Namespace: "ns", Name: "s"}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s.kind",
		},
		{
			name: "k8s no namespace",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "s",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s.namespace",
		},
		{
			name: "k8s labelSelector without namespace is cluster-wide",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion:    "snapshot.storage.k8s.io/v1",
					Kind:          "VolumeSnapshot",
					LabelSelector: "app=foo",
				}
			},
			wantErr: require.NoError,
		},
		{
			name: "k8s no name and no selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Namespace: "ns",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s",
		},
		{
			name: "k8s both name and selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion:    "snapshot.storage.k8s.io/v1",
					Kind:          "VolumeSnapshot",
					Namespace:     "ns",
					Name:          "s",
					LabelSelector: "app=foo",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s",
		},
		{
			name: "k8s select newestPerPVC with selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot",
					LabelSelector: "app=foo", Select: SelectNewestPerPVC,
				}
			},
			wantErr: require.NoError,
		},
		{
			name: "k8s select all with selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot",
					LabelSelector: "app=foo", Select: SelectAll,
				}
			},
			wantErr: require.NoError,
		},
		{
			name: "k8s select with name",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot",
					Namespace: "ns", Name: "s", Select: SelectNewestPerPVC,
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s.select",
		},
		{
			name: "k8s select unknown",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8s = &K8sRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot",
					LabelSelector: "app=foo", Select: "oldest",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8s.select",
		},
		{
			name: "zfsPath snapshot newest with prefix",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = &ZFSPathSource{Name: "pool/data", Snapshot: SnapshotNewest, Prefix: "daily-"}
			},
			wantErr: require.NoError,
		},
		{
			name: "zfsPath snapshot newest on a snapshot name",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = &ZFSPathSource{Name: "pool/data@snap", Snapshot: SnapshotNewest}
			},
			wantErr:     require.Error,
			errContains: "bare dataset",
		},
		{
			name: "zfsPath snapshot unknown",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = &ZFSPathSource{Name: "pool/data", Snapshot: "oldest"}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsPath.snapshot",
		},
		{
			name: "zfsPath prefix without snapshot",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = &ZFSPathSource{Name: "pool/data", Prefix: "daily-"}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsPath.prefix",
		},
		{
			name: "zfsProperty with root",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "tape.home.arpa:archive", Value: "true", Under: "pool"}}
			},
			wantErr: require.NoError,
		},
		{
			name: "zfsProperty with zfsPath",
			mutate: func(c *Config) {
				c.Sources[0].ZFSProperty = &ZFSPropertySource{Name: "tape:archive", Value: "true"}
			},
			wantErr:     require.Error,
			errContains: "exactly one of k8s, zfsPath or zfsProperty",
		},
		{
			name: "zfsProperty name without colon",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "archive", Value: "true"}}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsProperty.name",
		},
		{
			name: "zfsProperty name uppercase",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "Tape:Archive", Value: "true"}}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsProperty.name",
		},
		{
			name: "zfsProperty empty value",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "tape:archive"}}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsProperty.value",
		},
		{
			name: "zfsProperty value with newline",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "tape:archive", Value: "a\nb"}}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsProperty.value",
		},
		{
			name: "zfsProperty under a snapshot",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "tape:archive", Value: "true", Under: "pool@snap"}}
			},
			wantErr:     require.Error,
			errContains: "sources[0].zfsProperty.under",
		},
		{
			name: "zfsProperty with label",
			mutate: func(c *Config) {
				c.Sources[0] = Source{ZFSProperty: &ZFSPropertySource{Name: "tape:archive", Value: "true"}, Label: ptr("x")}
			},
			wantErr:     require.Error,
			errContains: "sources[0].label",
		},
		{
			name:        "copies zero",
			mutate:      func(c *Config) { c.Copies = 0 },
			wantErr:     require.Error,
			errContains: "copies",
		},
		{
			name: "copies exceeds drives is allowed",
			// Copies are written in successive drive-sets (issue #66).
			mutate:  func(c *Config) { c.Copies = 5; c.Library.BlankSlots = []int{1, 2, 3, 4, 5} },
			wantErr: require.NoError,
		},
		{
			name:        "library no changer",
			mutate:      func(c *Config) { c.Library.Changer = "" },
			wantErr:     require.Error,
			errContains: "library.changer",
		},
		{
			name:        "library no drives",
			mutate:      func(c *Config) { c.Library.Drives = nil },
			wantErr:     require.Error,
			errContains: "library.drives",
		},
		{
			name:        "library duplicate drives",
			mutate:      func(c *Config) { c.Library.Drives = []string{"/dev/nst0", "/dev/nst0"} },
			wantErr:     require.Error,
			errContains: "library.drives[1]",
		},
		{
			name:        "library blank drive",
			mutate:      func(c *Config) { c.Library.Drives = []string{"/dev/nst0", "  "} },
			wantErr:     require.Error,
			errContains: "library.drives[1]",
		},
		{
			name:    "library distinct non-blank drives is allowed",
			mutate:  func(c *Config) { c.Library.Drives = []string{"/dev/nst0", "/dev/nst1", "/dev/nst2"} },
			wantErr: require.NoError,
		},
		{
			name:        "library no blank slots",
			mutate:      func(c *Config) { c.Library.BlankSlots = nil },
			wantErr:     require.Error,
			errContains: "library.blankSlots",
		},
		{
			name:        "library duplicate blank slots",
			mutate:      func(c *Config) { c.Library.BlankSlots = []int{1030, 1030} },
			wantErr:     require.Error,
			errContains: "library.blankSlots[1]",
		},
		{
			name:        "library negative blank slot",
			mutate:      func(c *Config) { c.Library.BlankSlots = []int{1, -1} },
			wantErr:     require.Error,
			errContains: "library.blankSlots[1]",
		},
		{
			name:    "library distinct non-negative blank slots is allowed",
			mutate:  func(c *Config) { c.Library.BlankSlots = []int{0, 1, 2, 3} },
			wantErr: require.NoError,
		},
		{
			name:        "library blank slots not a multiple of copies",
			mutate:      func(c *Config) { c.Copies = 2; c.Library.BlankSlots = []int{1, 2, 3} },
			wantErr:     require.Error,
			errContains: "must be a positive multiple of copies",
		},
		{
			name:    "library blank slots a multiple of copies is allowed",
			mutate:  func(c *Config) { c.Copies = 3; c.Library.BlankSlots = []int{1, 2, 3, 4, 5, 6} },
			wantErr: require.NoError,
		},
		{
			name:        "library zero tape capacity",
			mutate:      func(c *Config) { c.Library.TapeCapacityBytes = 0 },
			wantErr:     require.Error,
			errContains: "library.tapeCapacityBytes",
		},
		{
			name:    "library io wait timeout set positive",
			mutate:  func(c *Config) { c.Library.IOWaitTimeoutSeconds = ptr(3600) },
			wantErr: require.NoError,
		},
		{
			name:        "library io wait timeout zero",
			mutate:      func(c *Config) { c.Library.IOWaitTimeoutSeconds = ptr(0) },
			wantErr:     require.Error,
			errContains: "library.ioWaitTimeoutSeconds",
		},
		{
			name:        "library io wait timeout negative",
			mutate:      func(c *Config) { c.Library.IOWaitTimeoutSeconds = ptr(-1) },
			wantErr:     require.Error,
			errContains: "library.ioWaitTimeoutSeconds",
		},
		{
			name:    "library write failure wait timeout set positive",
			mutate:  func(c *Config) { c.Library.WriteFailureWaitTimeoutSeconds = ptr(3600) },
			wantErr: require.NoError,
		},
		{
			name:        "library write failure wait timeout zero",
			mutate:      func(c *Config) { c.Library.WriteFailureWaitTimeoutSeconds = ptr(0) },
			wantErr:     require.Error,
			errContains: "library.writeFailureWaitTimeoutSeconds",
		},
		{
			name:        "library write failure wait timeout negative",
			mutate:      func(c *Config) { c.Library.WriteFailureWaitTimeoutSeconds = ptr(-1) },
			wantErr:     require.Error,
			errContains: "library.writeFailureWaitTimeoutSeconds",
		},
		{
			name:        "redundancy neither mode",
			mutate:      func(c *Config) { c.Redundancy.TargetPercentage = nil },
			wantErr:     require.Error,
			errContains: "redundancy",
		},
		{
			name: "redundancy both modes",
			mutate: func(c *Config) {
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 5}
			},
			wantErr:     require.Error,
			errContains: "redundancy",
		},
		{
			name: "redundancy negative percentage",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(-1.0)
			},
			wantErr:     require.Error,
			errContains: "redundancy.targetPercentage",
		},
		{
			name: "redundancy zero percentage",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(0.0)
			},
			wantErr:     require.Error,
			errContains: "redundancy.targetPercentage",
		},
		{
			name: "redundancy percentage at lower bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(1.0)
			},
			wantErr: require.NoError,
		},
		{
			name: "redundancy percentage at upper bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(100.0)
			},
			wantErr: require.NoError,
		},
		{
			name: "redundancy percentage above upper bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(101.0)
			},
			wantErr:     require.Error,
			errContains: "redundancy.targetPercentage",
		},
		{
			name: "redundancy percentage far above upper bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(150.0)
			},
			wantErr:     require.Error,
			errContains: "redundancy.targetPercentage",
		},
		{
			name: "redundancy fractional percentage",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = ptr(10.5)
			},
			wantErr:     require.Error,
			errContains: "redundancy.targetPercentage",
		},
		{
			name: "redundancy negative fill floor",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: -1}
			},
			wantErr:     require.Error,
			errContains: "redundancy.fillToCapacity.floor",
		},
		{
			name: "redundancy zero fill floor",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 0}
			},
			wantErr:     require.Error,
			errContains: "redundancy.fillToCapacity.floor",
		},
		{
			name: "redundancy fill floor at lower bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 1}
			},
			wantErr: require.NoError,
		},
		{
			name: "redundancy fill floor at upper bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 100}
			},
			wantErr: require.NoError,
		},
		{
			name: "redundancy fill floor above upper bound",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 101}
			},
			wantErr:     require.Error,
			errContains: "redundancy.fillToCapacity.floor",
		},
		{
			name: "redundancy fractional fill floor",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: 10.5}
			},
			wantErr:     require.Error,
			errContains: "redundancy.fillToCapacity.floor",
		},
		{
			name:        "no encryption recipients",
			mutate:      func(c *Config) { c.Encryption.Recipients = nil },
			wantErr:     require.Error,
			errContains: "encryption.recipients",
		},
		{
			name:        "blank encryption recipient",
			mutate:      func(c *Config) { c.Encryption.Recipients = []string{""} },
			wantErr:     require.Error,
			errContains: "encryption.recipients[0]",
		},
		{
			name:        "whitespace-only encryption recipient",
			mutate:      func(c *Config) { c.Encryption.Recipients = []string{"   "} },
			wantErr:     require.Error,
			errContains: "encryption.recipients[0]",
		},
		{
			name: "blank encryption recipient at index 1",
			mutate: func(c *Config) {
				c.Encryption.Recipients = []string{"age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4", "  "}
			},
			wantErr:     require.Error,
			errContains: "encryption.recipients[1]",
		},
		{
			name:        "no encryption identity",
			mutate:      func(c *Config) { c.Encryption.Identity = "" },
			wantErr:     require.Error,
			errContains: "encryption.identity",
		},
		{
			name:        "blank encryption identity",
			mutate:      func(c *Config) { c.Encryption.Identity = "   " },
			wantErr:     require.Error,
			errContains: "encryption.identity",
		},
		{
			name:    "feasibility overhead at the floor",
			mutate:  func(c *Config) { c.FeasibilityOverhead = ptr(1.0) },
			wantErr: require.NoError,
		},
		{
			name:        "feasibility overhead below one",
			mutate:      func(c *Config) { c.FeasibilityOverhead = ptr(0.99) },
			wantErr:     require.Error,
			errContains: "feasibilityOverhead",
		},
		{
			// No opticalBurn section: burning disabled, accepted.
			name:    "optical burn absent",
			mutate:  func(c *Config) { c.Delivery.OpticalBurn = nil },
			wantErr: require.NoError,
		},
		{
			// A present block that would burn nothing is rejected, not silently skipped (#386).
			name: "optical burn empty drives",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Copies: 2}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.drives",
		},
		{
			name: "optical burn zero copies",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 0}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.copies",
		},
		{
			name: "optical burn enabled",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 2}
			},
			wantErr: require.NoError,
		},
		{
			name: "optical burn negative copies",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: -1}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.copies",
		},
		{
			name: "optical burn blank drive path",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0", "  "}, Copies: 2}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.drives[1]",
		},
		{
			name: "optical burn duplicate drive path",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0", "/dev/sr0"}, Copies: 2}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.drives[1]",
		},
		{
			name: "optical burn burn wait timeout set positive",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{
					Drives: []string{"/dev/sr0"}, Copies: 2, BurnWaitTimeoutSeconds: ptr(3600),
				}
			},
			wantErr: require.NoError,
		},
		{
			name: "optical burn burn wait timeout zero",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{
					Drives: []string{"/dev/sr0"}, Copies: 2, BurnWaitTimeoutSeconds: ptr(0),
				}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.burnWaitTimeoutSeconds",
		},
		{
			name: "optical burn burn wait timeout negative",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{
					Drives: []string{"/dev/sr0"}, Copies: 2, BurnWaitTimeoutSeconds: ptr(-1),
				}
			},
			wantErr:     require.Error,
			errContains: "delivery.opticalBurn.burnWaitTimeoutSeconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			tt.wantErr(t, err)

			if tt.errContains != "" && err != nil {
				assert.Contains(t, err.Error(), tt.errContains)
			}
		})
	}
}

func TestEffectiveFeasibilityOverhead(t *testing.T) {
	t.Parallel()

	unset := Config{}
	assert.InDelta(t, DefaultFeasibilityOverhead, unset.EffectiveFeasibilityOverhead(), 1e-9)

	set := Config{FeasibilityOverhead: ptr(1.2)}
	assert.InDelta(t, 1.2, set.EffectiveFeasibilityOverhead(), 1e-9)
}

func TestEffectiveWriteFailureWaitTimeout(t *testing.T) {
	t.Parallel()

	var unset Library
	assert.Equal(t, DefaultWriteFailureWaitTimeout, unset.EffectiveWriteFailureWaitTimeout())

	set := Library{WriteFailureWaitTimeoutSeconds: ptr(90)}
	assert.Equal(t, 90*time.Second, set.EffectiveWriteFailureWaitTimeout())
}

func TestOpticalBurnEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		burn    *OpticalBurn
		enabled bool
	}{
		{name: "nil", burn: nil, enabled: false},
		{name: "empty drives", burn: &OpticalBurn{Copies: 2}, enabled: false},
		{name: "zero copies", burn: &OpticalBurn{Drives: []string{"/dev/sr0"}}, enabled: false},
		{name: "drives and copies", burn: &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 1}, enabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.enabled, tt.burn.Enabled())
		})
	}
}

func TestEffectiveBurnWaitTimeout(t *testing.T) {
	t.Parallel()

	var nilBurn *OpticalBurn
	assert.Equal(t, DefaultBurnWaitTimeout, nilBurn.EffectiveBurnWaitTimeout())

	unset := &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 1}
	assert.Equal(t, DefaultBurnWaitTimeout, unset.EffectiveBurnWaitTimeout())

	set := &OpticalBurn{BurnWaitTimeoutSeconds: ptr(90)}
	assert.Equal(t, 90*time.Second, set.EffectiveBurnWaitTimeout())
}

// findModuleRoot walks up from the test working directory to find go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root (go.mod not found)")
		}

		dir = parent
	}
}

func TestSchemaValidation(t *testing.T) {
	schemaPath := filepath.Join(findModuleRoot(t), "schemas", "run-config.schema.json")
	schemaBytes, err := os.ReadFile(schemaPath)
	require.NoError(t, err, "committed schema must exist; run: make generate-schema")

	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	require.NoError(t, err)

	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("run-config.schema.json", schemaDoc))
	sch, err := compiler.Compile("run-config.schema.json")
	require.NoError(t, err)

	goodConfig := validConfig()
	goodJSON, err := json.Marshal(goodConfig)
	require.NoError(t, err)

	var goodInst interface{}
	require.NoError(t, json.Unmarshal(goodJSON, &goodInst))
	assert.NoError(t, sch.Validate(goodInst), "known-good config must validate against schema")

	// missing: sources, library, redundancy, encryption, delivery
	var badInst interface{}
	require.NoError(t, json.Unmarshal([]byte(`{"copies": 2}`), &badInst))
	assert.Error(t, sch.Validate(badInst), "known-bad config must fail schema validation")

	modeConfig := validConfig()
	modeConfig.Sources = []Source{
		{ZFSPath: &ZFSPathSource{Name: "pool/data", Snapshot: SnapshotNewest, Prefix: "daily-"}},
		{K8s: &K8sRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", LabelSelector: "a=b", Select: SelectNewestPerPVC}},
		{ZFSProperty: &ZFSPropertySource{Name: "tape:archive", Value: "true", Under: "pool"}},
	}
	modeJSON, err := json.Marshal(modeConfig)
	require.NoError(t, err)

	var modeInst interface{}
	require.NoError(t, json.Unmarshal(modeJSON, &modeInst))
	assert.NoError(t, sch.Validate(modeInst), "snapshot selection modes must validate against schema")

	var badModeInst interface{}
	require.NoError(t, json.Unmarshal(bytes.Replace(modeJSON, []byte(`"newestPerPVC"`), []byte(`"oldest"`), 1), &badModeInst))
	assert.Error(t, sch.Validate(badModeInst), "unknown select mode must fail schema validation")
}
