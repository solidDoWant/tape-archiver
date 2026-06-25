package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

func validConfig() Config {
	return Config{
		Sources: []Source{
			{ZFSPath: &ZFSPathSource{Path: "bulk-pool-01/archive@snap-20240101"}},
		},
		Copies: 2,
		Library: Library{
			Changer:    "/dev/sch0",
			Drives:     []string{"/dev/nst0", "/dev/nst1"},
			BlankSlots: []int{1, 2},
		},
		Redundancy: Redundancy{
			TargetPercentage: ptr(10.0),
			SliceSizeBytes:   1 << 30,
		},
		Encryption: Encryption{
			Recipients: []string{"age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"},
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
		K8sSnapshot: &K8sSnapshot{
			Name:      "my-snapshot",
			Namespace: "default",
			Group:     true,
		},
	})
	original.Sources = append(original.Sources, Source{
		K8sSnapshot: &K8sSnapshot{LabelSelector: "app=myapp"},
	})
	original.Redundancy = Redundancy{
		FillToCapacity: &FillConfig{Floor: 5.0},
		SliceSizeBytes: 2 << 30,
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
				c.Sources[0].K8sSnapshot = &K8sSnapshot{Name: "s", Namespace: "ns"}
			},
			wantErr:     require.Error,
			errContains: "sources[0]",
		},
		{
			name:        "zfsPath empty path",
			mutate:      func(c *Config) { c.Sources[0].ZFSPath.Path = "" },
			wantErr:     require.Error,
			errContains: "sources[0].zfsPath.path",
		},
		{
			name: "k8s no name and no selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8sSnapshot = &K8sSnapshot{}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8sSnapshot",
		},
		{
			name: "k8s both name and selector",
			mutate: func(c *Config) {
				c.Sources[0].ZFSPath = nil
				c.Sources[0].K8sSnapshot = &K8sSnapshot{
					Name: "s", Namespace: "ns", LabelSelector: "app=foo",
				}
			},
			wantErr:     require.Error,
			errContains: "sources[0].k8sSnapshot",
		},
		{
			name:        "copies zero",
			mutate:      func(c *Config) { c.Copies = 0 },
			wantErr:     require.Error,
			errContains: "copies",
		},
		{
			name:        "copies exceeds drives",
			mutate:      func(c *Config) { c.Copies = 5 },
			wantErr:     require.Error,
			errContains: "copies",
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
			name: "redundancy negative fill floor",
			mutate: func(c *Config) {
				c.Redundancy.TargetPercentage = nil
				c.Redundancy.FillToCapacity = &FillConfig{Floor: -1}
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
