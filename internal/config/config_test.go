package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
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
			SliceSizeBytes:   1 << 30,
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
		SliceSizeBytes: 2 << 30,
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
			name:        "copies zero",
			mutate:      func(c *Config) { c.Copies = 0 },
			wantErr:     require.Error,
			errContains: "copies",
		},
		{
			// Copies may exceed the drive count: the tape path writes the copies
			// of each logical tape in successive drive-sets (issue #66).
			name:    "copies exceeds drives is allowed",
			mutate:  func(c *Config) { c.Copies = 5 },
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
			name:        "library no blank slots",
			mutate:      func(c *Config) { c.Library.BlankSlots = nil },
			wantErr:     require.Error,
			errContains: "library.blankSlots",
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
			name:        "redundancy zero slice size",
			mutate:      func(c *Config) { c.Redundancy.SliceSizeBytes = 0 },
			wantErr:     require.Error,
			errContains: "redundancy.sliceSizeBytes",
		},
		{
			name:        "no encryption recipients",
			mutate:      func(c *Config) { c.Encryption.Recipients = nil },
			wantErr:     require.Error,
			errContains: "encryption.recipients",
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
			// Present but no drives: disabled, still accepted.
			name: "optical burn empty drives",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Copies: 2}
			},
			wantErr: require.NoError,
		},
		{
			// Present with drives but zero copies: disabled, still accepted.
			name: "optical burn zero copies",
			mutate: func(c *Config) {
				c.Delivery.OpticalBurn = &OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 0}
			},
			wantErr: require.NoError,
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

	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("run-config.schema.json", bytes.NewReader(schemaBytes)))
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
}
