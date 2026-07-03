package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfigJSON is a minimal run config that passes both decoding and
// validation. Cases below mutate it to exercise specific failure modes.
const validConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:      "valid config",
			json:      validConfigJSON,
			assertErr: require.NoError,
		},
		{
			name:      "unknown field is rejected",
			json:      `{"sources": [], "copies": 2, "copys": 3}`,
			assertErr: require.Error,
		},
		{
			name:      "missing sources fails validation",
			json:      `{"copies": 2}`,
			assertErr: require.Error,
		},
		{
			// Copies exceeding the drive count is valid: the copies are written in
			// successive drive-sets (issue #66).
			name: "copies exceeding drives is accepted",
			json: `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 3,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2, 3], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`,
			assertErr: require.NoError,
		},
		{
			name:      "malformed JSON is rejected",
			json:      `{"sources": [`,
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Parse([]byte(test.json))
			test.assertErr(t, err)

			if err == nil {
				require.NotNil(t, cfg)
				assert.NoError(t, cfg.Validate())
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte(validConfigJSON), 0o600))

		cfg, err := LoadFile(path)
		require.NoError(t, err)
		assert.Equal(t, 2, cfg.Copies)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
		require.Error(t, err)
	})
}
