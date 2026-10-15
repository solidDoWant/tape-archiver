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

// logAPIKeyClaims debug-logs the JWT claims to identify the credential after
// rotation; never the signature, so logs cannot be replayed.
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

// decodeJWTClaims parses a JWT's claims without verifying the signature.
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

// extractAPIKeyFile returns the path of a "file:///abs/path" API key (clearing
// the inline value), or "" for an inline key. Non-canonical file: forms are
// rejected so typos are not sent as bearer tokens.
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
