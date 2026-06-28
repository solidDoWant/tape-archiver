package temporalclient

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/contrib/envconfig"
)

func TestExtractAPIKeyFile(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    string
		expectedKey string
		errFunc     require.ErrorAssertionFunc
	}{
		{
			name:        "empty api key passes through",
			input:       "",
			expected:    "",
			expectedKey: "",
		},
		{
			name:        "inline literal passes through",
			input:       "tmprl_abcdef",
			expected:    "",
			expectedKey: "tmprl_abcdef",
		},
		{
			name:        "file:// with absolute path is extracted and inline value cleared",
			input:       "file:///etc/temporal/api-key",
			expected:    "/etc/temporal/api-key",
			expectedKey: "",
		},
		{
			name:    "file:// with relative path is rejected",
			input:   "file://relative/path",
			errFunc: require.Error,
		},
		{
			name:    "file:// with empty path is rejected",
			input:   "file://",
			errFunc: require.Error,
		},
		{
			name:    "single-slash file: form is rejected",
			input:   "file:/etc/key",
			errFunc: require.Error,
		},
		{
			name:    "bare file: scheme without slashes is rejected",
			input:   "file:",
			errFunc: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			profile := envconfig.ClientConfigProfile{APIKey: test.input}

			got, err := extractAPIKeyFile(&profile)
			test.errFunc(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.expected, got)
			assert.Equal(t, test.expectedKey, profile.APIKey)
		})
	}
}

func TestDecodeJWTClaims(t *testing.T) {
	validJWT := func(t *testing.T, claims map[string]any) string {
		t.Helper()

		payload, err := json.Marshal(claims)
		require.NoError(t, err)

		// Use placeholder header and signature segments — decodeJWTClaims only
		// inspects the middle segment, so the other two need only be present
		// and base64-url decodable to the parser. The signature is intentionally
		// an arbitrary value: this code path never verifies it.
		return strings.Join([]string{
			base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)),
			base64.RawURLEncoding.EncodeToString(payload),
			base64.RawURLEncoding.EncodeToString([]byte("placeholder-signature")),
		}, ".")
	}

	t.Run("valid JWT returns claims", func(t *testing.T) {
		claims := map[string]any{
			"sub":   "service@tenant",
			"iss":   "https://login.example/",
			"aud":   "https://api.example/",
			"exp":   float64(1735689600), // JSON numbers decode as float64
			"scope": "workflow:write",
		}

		got, err := decodeJWTClaims(validJWT(t, claims))
		require.NoError(t, err)
		assert.Equal(t, claims, got)
	})

	tests := []struct {
		name        string
		input       string
		errContains string
	}{
		{name: "empty string", input: "", errContains: "expected 3"},
		{name: "no dots (opaque key)", input: "tmprl_abcdef", errContains: "expected 3"},
		{name: "two segments", input: "header.payload", errContains: "expected 3"},
		{name: "four segments", input: "a.b.c.d", errContains: "expected 3"},
		{name: "middle segment is not base64url", input: "header.!!!.signature", errContains: "base64-decode"},
		{name: "middle segment is base64 of non-JSON", input: "header." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".sig", errContains: "unmarshal"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeJWTClaims(test.input)
			require.Error(t, err)
			assert.ErrorContains(t, err, test.errContains)
			assert.Nil(t, got)
		})
	}
}
