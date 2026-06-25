package envvar

import "os"

// Config holds configuration parsed from environment variables.
type Config struct {
	// DiscordFailureWebhookURL is the webhook URL for run failure alerts.
	// When empty, failure alerting is disabled — no error is raised on absence.
	DiscordFailureWebhookURL string
}

// Parse reads operational configuration from environment variables.
func Parse() (Config, error) {
	return Config{
		DiscordFailureWebhookURL: os.Getenv("DISCORD_FAILURE_WEBHOOK_URL"),
	}, nil
}
