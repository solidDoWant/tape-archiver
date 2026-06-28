package temporalclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"go.temporal.io/sdk/contrib/envconfig"
)

const (
	apiKeyFileScheme = "file:"
	apiKeyFilePrefix = "file://"
)

func apiKeyFileCallback(apiKeyFile string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		key, err := readAPIKeyFile(apiKeyFile)
		if err != nil {
			return "", err
		}

		logAPIKeyClaims(ctx, apiKeyFile, key)

		return key, nil
	}
}

// readAPIKeyFile reads, trims, and rejects empty content — a transient empty
// state during external rotation would otherwise send an empty bearer token.
func readAPIKeyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read api key file %q: %w", path, err)
	}

	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("api key file %q is empty", path)
	}

	return key, nil
}

// logAPIKeyClaims emits the decoded JWT claims at debug level so an operator
// can confirm which credential is in use after a rotation. Decode failures
// log at debug level; the token still flows to the server. Only the claims
// payload is logged — never the signature — so logs can't be replayed.
func logAPIKeyClaims(ctx context.Context, path, apiKey string) {
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}

	claims, err := decodeJWTClaims(apiKey)
	if err != nil {
		slog.DebugContext(ctx, "temporal api key is not a decodable JWT",
			slog.String("path", path),
			slog.String("err", err.Error()),
		)

		return
	}

	slog.DebugContext(ctx, "loaded temporal api key", slog.String("path", path), slog.Any("claims", claims))
}

// decodeJWTClaims parses the claims segment of a JWS-Compact JWT. It does
// not verify the signature — the caller has already chosen to trust the
// source.
func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected 3 dot-separated segments, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("base64-decode claims segment: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims as JSON object: %w", err)
	}

	return claims, nil
}

// extractAPIKeyFile detects a "file:///abs/path" API key and clears the
// inline value on the profile so envconfig's static-credentials path is
// bypassed. Returns the file path, or "" when the key is inline.
// Non-canonical forms like "file:/path" are rejected so typos aren't silently
// sent as bearer tokens.
func extractAPIKeyFile(profile *envconfig.ClientConfigProfile) (string, error) {
	if !strings.HasPrefix(profile.APIKey, apiKeyFileScheme) {
		return "", nil
	}

	path, ok := strings.CutPrefix(profile.APIKey, apiKeyFilePrefix)
	if !ok {
		return "", fmt.Errorf("temporal api key %q: file URIs must use the canonical form (file:///path/to/key)", profile.APIKey)
	}

	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("temporal api key %q: file:// path must be absolute (file:///path/to/key)", profile.APIKey)
	}

	profile.APIKey = ""

	return path, nil
}
