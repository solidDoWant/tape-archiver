//go:build linux

package tape

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreventAllowCDB(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prevent bool
		want    []byte
	}{
		"allow clears the PREVENT field": {prevent: false, want: []byte{0x1E, 0, 0, 0, 0x00, 0}},
		"prevent sets PREVENT=01b":       {prevent: true, want: []byte{0x1E, 0, 0, 0, 0x01, 0}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, preventAllowCDB(test.prevent))
		})
	}
}

func TestAllowMediumRemovalErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		device  func(t *testing.T) string
		wantErr string
	}{
		"missing node names the device": {
			device:  func(t *testing.T) string { return filepath.Join(t.TempDir(), "sg-missing") },
			wantErr: "sg-missing",
		},
		// /dev/null opens but rejects SG_IO, so the failure names the command.
		"non-sg node names the command": {
			device:  func(*testing.T) string { return "/dev/null" },
			wantErr: "ALLOW MEDIUM REMOVAL",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := AllowMediumRemoval(t.Context(), test.device(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}
