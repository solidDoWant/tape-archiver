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
