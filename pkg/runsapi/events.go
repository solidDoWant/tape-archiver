// GET /api/events/runs/{runID} streams run detail over SSE. Every connection
// watching a run shares one server-side poll loop (runBroker), so Temporal load
// is one interval per run, not per open tab.
package runsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
)

// eventPollInterval is a var so tests can shorten it; brokers capture it at
// construction so poll goroutines never race the tests' restore.
var eventPollInterval = 2 * time.Second

// sseWriteTimeout bounds a single SSE write+flush. The server WriteTimeout is
// cleared for streams, so without this a TCP-backpressured client could block a
// write forever and stall graceful shutdown (issue #270).
const sseWriteTimeout = 15 * time.Second

// sseHeartbeatInterval keeps idle streams alive through proxies with idle
// timeouts (kept under the common 60s). A var so tests can shorten it; handlers
// capture it at construction.
var sseHeartbeatInterval = 25 * time.Second

// runningStatus is the only non-terminal status; any other ends the stream.
var runningStatus = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String()

// SSE event names streamRunEvents emits.
const (
	// sseEventUpdate carries a RunDetail, sent first and then only when
	// status, phase, or pause state changes.
	sseEventUpdate = "update"
	// sseEventDone repeats the final RunDetail once the run is terminal, so
	// the client knows to stop reconnecting.
	sseEventDone = "done"
)

// streamRunEvents implements GET /api/events/runs/{runID}. The first fetch
// happens before switching to text/event-stream so an unknown run gets a real
// HTTP error status (EventSource would otherwise only see an opaque connection
// error). Pause state is part of the delta check because a pause can start or
// clear without Status or LastCompletedPhase changing.
func (h *handler) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	if runID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("runID is required"))

		return
	}

	detail, err := h.fetchRunDetailUntilDrain(r.Context(), runID)
	if err != nil {
		writeRunDetailError(w, runID, err)

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Stops nginx-style proxies from buffering the stream into one burst.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The server's WriteTimeout is an absolute deadline set at header read and
	// would kill a long-lived stream; writes set per-write deadlines instead
	// (sseWriteTimeout, issue #270).
	responseController := http.NewResponseController(w)

	if !writeSSEEvent(w, responseController, sseEventUpdate, detail) {
		return
	}

	if detail.Status != runningStatus {
		writeSSEEvent(w, responseController, sseEventDone, detail)

		return
	}

	last := detail

	// Delta detection is per subscriber, against what this connection last sent.
	sub := h.broker.subscribe(runID)
	defer h.broker.unsubscribe(sub)

	heartbeat := time.NewTicker(h.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !writeSSEComment(w, responseController) {
				return
			}
		case <-h.drain.Done():
			// Close on server drain so Shutdown is not held open (issue #270);
			// EventSource reconnects to the successor pod.
			return
		case next := <-sub.updates:
			if next.LastCompletedPhaseUnknown {
				// A transient query failure must not look like a phase change:
				// carry the last known phase (and its flag) forward.
				next.LastCompletedPhase = last.LastCompletedPhase
				next.LastCompletedPhaseUnknown = last.LastCompletedPhaseUnknown
			}

			if next.CurrentPause.Unknown {
				// Likewise, or Resume/Abort would vanish on a blip.
				next.CurrentPause = last.CurrentPause
			}

			if next.Status == last.Status &&
				next.LastCompletedPhase == last.LastCompletedPhase &&
				next.LastCompletedPhaseUnknown == last.LastCompletedPhaseUnknown &&
				currentPauseEqual(next.CurrentPause, last.CurrentPause) {
				continue
			}

			last = next

			if !writeSSEEvent(w, responseController, sseEventUpdate, next) {
				return
			}

			if next.Status != runningStatus {
				writeSSEEvent(w, responseController, sseEventDone, next)

				return
			}
		}
	}
}

// currentPauseEqual treats nil and empty slices as equal (unlike
// reflect.DeepEqual), since the pause query can return either for the same state.
func currentPauseEqual(a, b CurrentPauseInfo) bool {
	return a.Kind == b.Kind &&
		a.Phase == b.Phase &&
		a.AwaitingExport == b.AwaitingExport &&
		a.ErrorSummary == b.ErrorSummary &&
		a.CanAbort == b.CanAbort &&
		a.Unknown == b.Unknown &&
		slices.Equal(a.AffectedTapes, b.AffectedTapes) &&
		slices.Equal(a.ReloadSlots, b.ReloadSlots) &&
		slices.Equal(a.Devices, b.Devices)
}

// fetchRunDetailUntilDrain also cancels the fetch when the server drains, so a
// slow Temporal RPC cannot hold up Shutdown (issue #270).
func (h *handler) fetchRunDetailUntilDrain(ctx context.Context, runID string) (RunDetail, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	stop := context.AfterFunc(h.drain, cancel)
	defer stop()

	return fetchRunDetail(fetchCtx, h.temporalClient, runID)
}

// runBroker runs one poll loop per watched run, started by the first subscriber
// and stopped when the last leaves (issue #250).
type runBroker struct {
	fetch func(ctx context.Context, runID string) (RunDetail, error)
	drain context.Context

	// pollInterval is captured at construction to avoid racing tests that
	// override and restore eventPollInterval.
	pollInterval time.Duration

	// mu guards polls and every runPoll within it.
	mu    sync.Mutex
	polls map[string]*runPoll
}

// runPoll is the shared poll loop and subscriber set for one run.
type runPoll struct {
	runID       string
	stop        context.CancelFunc
	subscribers map[*subscriber]struct{}
	// terminal caches the final RunDetail so a late subscriber still gets a
	// done frame instead of hanging.
	terminal *RunDetail
}

// subscriber receives updates on a size-1, latest-wins channel so a slow
// consumer never blocks the broadcaster.
type subscriber struct {
	poll    *runPoll
	updates chan RunDetail
}

func newRunBroker(fetch func(context.Context, string) (RunDetail, error), drain context.Context) *runBroker {
	return &runBroker{
		fetch:        fetch,
		drain:        drain,
		pollInterval: eventPollInterval,
		polls:        make(map[string]*runPoll),
	}
}

func (b *runBroker) subscribe(runID string) *subscriber {
	b.mu.Lock()
	defer b.mu.Unlock()

	poll := b.polls[runID]
	if poll == nil {
		ctx, cancel := context.WithCancel(context.Background())
		poll = &runPoll{
			runID:       runID,
			stop:        cancel,
			subscribers: make(map[*subscriber]struct{}),
		}
		b.polls[runID] = poll

		go b.run(ctx, poll)
	}

	sub := &subscriber{poll: poll, updates: make(chan RunDetail, 1)}
	poll.subscribers[sub] = struct{}{}

	if poll.terminal != nil {
		// The channel is freshly made and empty, so this never blocks.
		sub.updates <- *poll.terminal
	}

	return sub
}

func (b *runBroker) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	poll := sub.poll
	delete(poll.subscribers, sub)

	if len(poll.subscribers) > 0 {
		return
	}

	poll.stop()

	// Don't delete a fresh poll a concurrent subscribe installed.
	if b.polls[poll.runID] == poll {
		delete(b.polls, poll.runID)
	}
}

func (b *runBroker) run(ctx context.Context, poll *runPoll) {
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.drain.Done():
			return
		case <-ticker.C:
			detail, err := b.fetch(ctx, poll.runID)
			if err != nil {
				// Transient: retry next tick rather than tear down the stream.
				slog.WarnContext(ctx, "runsapi: poll for run event stream failed", "run_id", poll.runID, "error", err)

				continue
			}

			terminal := detail.Status != runningStatus
			b.broadcast(poll, detail, terminal)

			if terminal {
				// The poll entry keeps the terminal detail so late subscribers get a done frame.
				return
			}
		}
	}
}

func (b *runBroker) broadcast(poll *runPoll, detail RunDetail, terminal bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if terminal {
		final := detail
		poll.terminal = &final
	}

	for sub := range poll.subscribers {
		sendLatest(sub.updates, detail)
	}
}

// sendLatest replaces any pending update without blocking. Only the broker
// (under b.mu) sends, so drain-then-send is race-free.
func sendLatest(ch chan RunDetail, detail RunDetail) {
	select {
	case ch <- detail:
	default:
		select {
		case <-ch:
		default:
		}

		select {
		case ch <- detail:
		default:
		}
	}
}

// writeSSEEvent writes and flushes one event, reporting false if the client is
// gone.
func writeSSEEvent(w http.ResponseWriter, responseController *http.ResponseController, event string, body interface{}) bool {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("runsapi: encode SSE event failed", "event", event, "error", err)

		return false
	}

	// ErrNotSupported is fine to ignore.
	_ = responseController.SetWriteDeadline(time.Now().Add(sseWriteTimeout))

	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}

	if err := responseController.Flush(); err != nil {
		return false
	}

	return true
}

// writeSSEComment writes a keepalive comment, which EventSource ignores.
func writeSSEComment(w http.ResponseWriter, responseController *http.ResponseController) bool {
	_ = responseController.SetWriteDeadline(time.Now().Add(sseWriteTimeout))

	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return false
	}

	return responseController.Flush() == nil
}
