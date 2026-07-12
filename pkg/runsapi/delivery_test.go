package runsapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestDeriveDeliveryMessageURL covers issue #306's reconstruction: the run
// overview's Discord deep-link is built from the completed Deliver activity's
// recorded result (backup.DeliverResult), and is empty whenever there is nothing
// linkable — an incomplete identity, a failed delivery, or no Deliver at all.
func TestDeriveDeliveryMessageURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		deliver func(t *testing.T, b *eventBuilder)
		want    string
	}{
		{
			name: "full identity builds the jump-to-message link",
			deliver: func(t *testing.T, b *eventBuilder) {
				id := b.scheduled(t, "Deliver", nil)
				b.completed(t, id, backup.DeliverResult{GuildID: "g1", ChannelID: "c1", MessageID: "m1"})
			},
			want: "https://discord.com/channels/g1/c1/m1",
		},
		{
			name: "a missing guild yields no link",
			deliver: func(t *testing.T, b *eventBuilder) {
				id := b.scheduled(t, "Deliver", nil)
				b.completed(t, id, backup.DeliverResult{ChannelID: "c1", MessageID: "m1"})
			},
			want: "",
		},
		{
			name: "a failed delivery yields no link",
			deliver: func(t *testing.T, b *eventBuilder) {
				id := b.scheduled(t, "Deliver", nil)
				b.failed(id, "webhook: unexpected status 500")
			},
			want: "",
		},
		{
			name:    "no delivery activity yields no link",
			deliver: func(_ *testing.T, _ *eventBuilder) {},
			want:    "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			b := newEventBuilder()
			b.started(t, testConfig)
			test.deliver(t, b)
			b.runCompleted()

			fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{events: b.events}
			}}

			history, err := fetchRunHistory(t.Context(), fake, "run-1")
			require.NoError(t, err)

			assert.Equal(t, test.want, deriveDeliveryMessageURL(history.Activities))
		})
	}
}

// TestGetRunDeliveryHandler covers the GET /api/runs/{runID}/delivery wiring: a
// run whose Deliver recorded a full message identity reports the assembled Discord
// deep-link.
func TestGetRunDeliveryHandler(t *testing.T) {
	t.Parallel()

	b := newEventBuilder()
	b.started(t, testConfig)
	deliver := b.scheduled(t, "Deliver", nil)
	b.completed(t, deliver, backup.DeliverResult{GuildID: "g1", ChannelID: "c1", MessageID: "m1"})
	b.runCompleted()

	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: b.events}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/delivery", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunDeliveryResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "run-1", body.RunID)
	assert.Equal(t, "https://discord.com/channels/g1/c1/m1", body.MessageURL)
}
