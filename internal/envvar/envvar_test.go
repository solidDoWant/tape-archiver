package envvar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_withWebhookURL(t *testing.T) {
	t.Setenv("DISCORD_FAILURE_WEBHOOK_URL", "https://discord.com/api/webhooks/test")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Equal(t, "https://discord.com/api/webhooks/test", cfg.DiscordFailureWebhookURL)
}

func TestParse_withoutWebhookURL(t *testing.T) {
	t.Setenv("DISCORD_FAILURE_WEBHOOK_URL", "")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Empty(t, cfg.DiscordFailureWebhookURL)
}

func TestParse_withStagingDir(t *testing.T) {
	t.Setenv("TAPE_STAGING_DIR", "/mnt/bulk-pool-01/archive/.tape-staging")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Equal(t, "/mnt/bulk-pool-01/archive/.tape-staging", cfg.StagingDir)
}

func TestParse_withoutStagingDir(t *testing.T) {
	t.Setenv("TAPE_STAGING_DIR", "")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Empty(t, cfg.StagingDir)
}

func TestParse_withRecoveryBinariesDir(t *testing.T) {
	t.Setenv("TAPE_RECOVERY_BINARIES_DIR", "/opt/recovery-bin")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Equal(t, "/opt/recovery-bin", cfg.RecoveryBinariesDir)
}

func TestParse_withoutRecoveryBinariesDir(t *testing.T) {
	t.Setenv("TAPE_RECOVERY_BINARIES_DIR", "")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Empty(t, cfg.RecoveryBinariesDir)
}

func TestParse_withRecoverySourcesDir(t *testing.T) {
	t.Setenv("TAPE_RECOVERY_SOURCES_DIR", "/opt/recovery-src")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Equal(t, "/opt/recovery-src", cfg.RecoverySourcesDir)
}

func TestParse_withoutRecoverySourcesDir(t *testing.T) {
	t.Setenv("TAPE_RECOVERY_SOURCES_DIR", "")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.Empty(t, cfg.RecoverySourcesDir)
}
