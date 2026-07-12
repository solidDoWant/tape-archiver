package main

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// TestHandler_DriveTheRealWebhookClient exercises the fake receiver through the
// real pkg/webhook.Client — the exact component that talks to it in production
// — so the test fails if the mock ever stops satisfying the contract
// SendFile/Send/FetchWebhookGuild depend on. Per CLAUDE.md's testing style it
// drives the public interface, never the handler's internals.
func TestHandler_DriveTheRealWebhookClient(t *testing.T) {
	server := httptest.NewServer(newHandler("guild-1", "channel-1"))
	defer server.Close()

	client := webhook.New(server.URL + "/webhook/dev")

	t.Run("FetchWebhookGuild returns the configured guild", func(t *testing.T) {
		guild, err := client.FetchWebhookGuild(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "guild-1", guild)
	})

	t.Run("SendFile returns a resolvable posted-message identity", func(t *testing.T) {
		path := writeTempFile(t, "sample report body")

		posted, err := client.SendFile(t.Context(), path)
		require.NoError(t, err)
		require.NotNil(t, posted, "the report deep-link needs a message identity")
		assert.Equal(t, "channel-1", posted.ChannelID)
		assert.NotEmpty(t, posted.ID)
	})

	t.Run("each SendFile yields a distinct message id", func(t *testing.T) {
		path := writeTempFile(t, "another report")

		first, err := client.SendFile(t.Context(), path)
		require.NoError(t, err)
		require.NotNil(t, first)

		second, err := client.SendFile(t.Context(), path)
		require.NoError(t, err)
		require.NotNil(t, second)

		assert.NotEqual(t, first.ID, second.ID, "distinct deliveries must produce distinct deep-links")
	})

	t.Run("Send (a no-wait alert) succeeds", func(t *testing.T) {
		// A non-2xx would surface as an error here; the fake answers 204.
		err := client.Send(t.Context(), webhook.Message{Content: "run paused"})
		require.NoError(t, err)
	})
}

func writeTempFile(t *testing.T, contents string) string {
	t.Helper()

	path := t.TempDir() + "/report.pdf"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}
