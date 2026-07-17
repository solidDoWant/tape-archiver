// This file implements GET /api/events/runs/{runID}: a Server-Sent Events
// (text/event-stream) view over the same run detail getRun serves, pushed
// as the server polls Temporal rather than left to client-side polling
// (docs/web-ui-design.md §3's "no client polling storms" — the polling still
// happens, but here, server-side, at one interval, regardless of how many
// browser tabs are watching). That last property is delivered by runBroker
// (below): every connection watching a given run shares one poll loop, so the
// Temporal load is one interval per run, not one per open tab.
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

// eventPollInterval is how often streamRunEvents re-polls Temporal for a
// run's current status/phase. Short enough that an operator watching a run
// sees a change promptly, long enough that moving the polling server-side
// does not just relocate a polling storm rather than actually taming it.
//
// A var, not a const: pkg/runsapi's tests override it (see
// setEventPollInterval in events_test.go) to exercise the poll loop's
// change-detection and terminal-close behavior in milliseconds instead of
// tying every test run to the real production interval.
var eventPollInterval = 2 * time.Second

// sseWriteTimeout bounds a single SSE write+flush. The server-wide
// http.Server WriteTimeout is cleared for this response (it is an absolute
// deadline set once at header-read, which would kill an arbitrarily
// long-lived stream mid-flight), but leaving the write deadline off entirely
// lets a TCP-backpressured client block a write indefinitely — and a
// goroutine blocked inside a write never reaches the select that observes the
// drain context, so a single stalled client would hold http.Server.Shutdown
// open to its full deadline (issue #270). A per-write deadline, reset before
// each write, bounds that: a healthy but idle stream is never killed (the
// deadline only spans an actual write, not the wait between them), while a
// stalled write fails after this window and unwinds the goroutine.
const sseWriteTimeout = 15 * time.Second

// sseHeartbeatInterval bounds how long a quiescent (running but unchanging) run
// stream can go without sending any bytes. streamRunEvents only writes on an
// actual state delta, so a long-idle run would otherwise send nothing at all —
// and an intermediary (load balancer / reverse proxy) with an idle-connection
// timeout shorter than the run's next state change would silently drop the
// stream. A periodic SSE comment (":\n\n" — ignored by EventSource, never
// surfaced as an event) keeps the connection demonstrably alive. Chosen well
// under common 60s proxy idle timeouts; a dropped stream is recoverable
// (EventSource reconnects), so this is belt-and-suspenders, not correctness.
//
// A var, not a const, for the same reason as eventPollInterval: the package's
// tests override it (setSSEHeartbeatInterval) to exercise the keepalive in
// milliseconds instead of the real production interval.
var sseHeartbeatInterval = 25 * time.Second

// runningStatus is the string form of WORKFLOW_EXECUTION_STATUS_RUNNING, as
// stored in RunSummary.Status (toRunSummary uses execution.GetStatus().String()).
// Any other status is terminal for the purposes of this stream: the run will
// not produce any further state change, so streamRunEvents ends the stream
// once it observes one.
var runningStatus = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String()

// SSE event names streamRunEvents emits.
const (
	// sseEventUpdate carries a RunDetail JSON body, sent on the first
	// successful poll and again every time the polled status, last completed
	// phase, or current pause state actually changes from what was last
	// sent — never on a poll tick that observed no change, so a quiescent run
	// does not turn into an endless stream of no-op events.
	sseEventUpdate = "update"
	// sseEventDone carries the same final RunDetail body as the "update"
	// event immediately before it, and is sent exactly once, right before
	// the handler returns (closing the stream) because the run reached a
	// terminal status. A client can use this to distinguish "the run
	// finished, stop reconnecting" from any other reason the connection
	// might drop (network blip, server restart, tab closed).
	sseEventDone = "done"
)

// streamRunEvents implements GET /api/events/runs/{runID}. Its very first
// poll (fetchRunDetail, the same call getRun makes) decides whether this
// request becomes an SSE stream at all: an unknown or malformed run ID is
// reported as a normal JSON error response with a real HTTP status — exactly
// like getRun — rather than a 200 text/event-stream that immediately fails
// inside the stream body, which a browser's EventSource would otherwise
// only ever report as an opaque connection error, with no status code or
// message surfaced to the page.
//
// Once a run is confirmed to exist, the response becomes text/event-stream
// and streamRunEvents subscribes to the run's shared poll loop (runBroker),
// which fetches run detail every eventPollInterval on behalf of every
// connection watching that run. This connection writes an "update" event only
// when the polled status, phase, or current pause state differs from what IT
// last sent — the last is what makes an
// operator-in-the-loop pause (workflows/backup.CurrentPauseQuery) starting or
// clearing show up live, even though neither Status nor LastCompletedPhase
// necessarily changes at the moment a pause begins or a resume/abort clears
// it (SPEC §4.3; docs/web-ui-design.md §3's pause-actions sketch). If the
// run's status is (or becomes) anything other than RUNNING, it writes one
// final "done" event and returns, closing the stream — this handler never
// polls a finished run forever. It also returns promptly once r.Context() is
// done (the client disconnected): there is no server-side per-client state
// beyond this one goroutine's stack, so there is nothing left to clean up
// either way.
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
	// Not every deployment topology sits behind nginx (docs/web-ui-design.md
	// §5's Ingress model may or may not), but the header is a no-op wherever
	// it's not understood and prevents a real, previously-seen class of bug
	// (a reverse proxy buffering the whole response before forwarding it,
	// which turns a "live" stream into one big delayed burst) wherever it is.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// cmd/web's http.Server sets a WriteTimeout to guard ordinary handlers
	// against a stalled client, but that deadline is computed once, when the
	// request's headers are read — not reset on each Write — so an
	// arbitrarily long-lived SSE response would otherwise be killed
	// mid-stream the moment the server's WriteTimeout elapses, regardless of
	// how active the stream still is. http.ResponseController.SetWriteDeadline
	// is the net/http-documented way to override that deadline for one
	// specific response without touching the server-wide setting every other
	// handler still relies on. writeSSEEvent below resets a fresh, bounded
	// per-write deadline (sseWriteTimeout) before each write, so an idle stream
	// is never killed for being idle, but a stalled write cannot block forever
	// and defeat the drain-driven shutdown (issue #270).
	responseController := http.NewResponseController(w)

	if !writeSSEEvent(w, responseController, sseEventUpdate, detail) {
		return
	}

	if detail.Status != runningStatus {
		writeSSEEvent(w, responseController, sseEventDone, detail)

		return
	}

	last := detail

	// Subscribe to the run's shared poll loop rather than polling Temporal from
	// this connection: every browser tab watching the same run rides one
	// server-side poller, so N tabs cost one interval of Temporal load, not N
	// (issue #250 review — this is what actually makes the polling "one interval
	// regardless of how many tabs"). Each subscriber still runs its own delta
	// detection against what IT last sent, so connections that joined at
	// different points stay individually correct.
	sub := h.broker.subscribe(runID)
	defer h.broker.unsubscribe(sub)

	// Emit a periodic comment so a run that stays RUNNING without any state
	// change still sends bytes, keeping idle-timeout intermediaries from dropping
	// the stream (see sseHeartbeatInterval).
	heartbeat := time.NewTicker(sseHeartbeatInterval)
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
			// The hosting server has begun graceful shutdown
			// (WithDrainContext). Close the stream now so
			// http.Server.Shutdown can reach connection quiescence instead
			// of stalling on this open response until its deadline (issue
			// #270). The browser's EventSource treats the closed connection
			// like any other drop and reconnects — to this pod's successor,
			// in a rolling deploy.
			return
		case next := <-sub.updates:
			if next.lastCompletedPhaseUnknown {
				// LastCompletedPhaseQuery failed on the poll that produced this
				// update (Describe still succeeded). Carry the last known phase
				// forward rather than letting "" reach the delta check below,
				// which would emit a spurious "phase regressed to empty" update
				// that flaps back on the next successful poll — the same
				// transient-blip handling CurrentPause.Unknown gets just below.
				next.LastCompletedPhase = last.LastCompletedPhase
			}

			if next.CurrentPause.Unknown {
				// CurrentPauseQuery itself failed on that poll (fetchRunDetail
				// still succeeded overall — Describe/LastCompletedPhase are
				// independently valid). Carry the last known pause state
				// forward rather than comparing against it: an unknown result
				// must never look like "the pause cleared" to the delta check
				// below, or a run genuinely still awaiting an operator would
				// flash Resume/Abort away on a transient blip. If Status or
				// LastCompletedPhase did change, the event below still fires —
				// just with the last known (not fabricated-healthy) pause state.
				next.CurrentPause = last.CurrentPause
			}

			if next.Status == last.Status &&
				next.LastCompletedPhase == last.LastCompletedPhase &&
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

// currentPauseEqual reports whether two CurrentPauseInfo values are the same
// pause state, for streamRunEvents' delta check. It replaces reflect.DeepEqual,
// which treats a nil slice and an empty non-nil slice as different:
// backup.CurrentPauseQuery can return either for the same logical "no affected
// tapes / reload slots / devices" state across polls, and DeepEqual would then
// report a change and emit a spurious "update" frame on a tick where nothing
// actually changed — defeating the "never on a no-op tick" guarantee. Scalars
// compare directly; slices use slices.Equal, which is length-then-elementwise
// so nil and empty (both length zero) are equal.
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

// fetchRunDetailUntilDrain is streamRunEvents' fetchRunDetail wrapper: the
// usual requestTimeout bound, plus cancellation the moment the drain context
// (WithDrainContext) ends. Without the latter, a Temporal RPC in flight when
// graceful shutdown begins would hold the stream — and with it
// http.Server.Shutdown — for up to the full requestTimeout (issue #270; a
// slow or unresponsive Temporal makes this real, not theoretical). A
// drain-aborted fetch surfaces as an error to the poll loop, whose next
// select iteration hits the drain case and ends the stream.
func (h *handler) fetchRunDetailUntilDrain(ctx context.Context, runID string) (RunDetail, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	stop := context.AfterFunc(h.drain, cancel)
	defer stop()

	return fetchRunDetail(fetchCtx, h.temporalClient, runID)
}

// --- shared per-run poller ---
//
// runBroker coalesces every SSE connection watching the same run onto one
// server-side poll loop, so N browser tabs cost one interval of Temporal load,
// not N (issue #250 review). streamRunEvents subscribes; the broker starts a
// poll goroutine on the first subscriber for a run and stops it when the last
// one leaves. Each poll's RunDetail is broadcast to every current subscriber,
// which does its own delta detection — the poller itself keeps no per-connection
// "last sent" state.

type runBroker struct {
	fetch func(ctx context.Context, runID string) (RunDetail, error)
	drain context.Context

	// mu guards polls and every field of the runPolls within it. A single lock
	// keeps subscribe, unsubscribe, and broadcast free of any lock-ordering
	// hazard; broadcast holds it only briefly, once per poll interval.
	mu    sync.Mutex
	polls map[string]*runPoll
}

// runPoll is the shared poll loop and subscriber set for one run.
type runPoll struct {
	runID       string
	stop        context.CancelFunc
	subscribers map[*subscriber]struct{}
	// terminal holds the final RunDetail once the run reaches a terminal status,
	// so a connection that subscribes after the poller has already stopped (a
	// rare Describe-lag race) still receives a done frame instead of hanging.
	terminal *RunDetail
}

// subscriber is one SSE connection's view of a run's shared poller: a size-1,
// latest-wins channel of RunDetail updates. A slow consumer coalesces to the
// newest state rather than backing the broadcaster up.
type subscriber struct {
	poll    *runPoll
	updates chan RunDetail
}

func newRunBroker(fetch func(context.Context, string) (RunDetail, error), drain context.Context) *runBroker {
	return &runBroker{fetch: fetch, drain: drain, polls: make(map[string]*runPoll)}
}

// subscribe registers a new SSE connection for runID, starting the shared poll
// loop if this is the first subscriber for that run.
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

// unsubscribe removes a connection; when a run's last subscriber leaves, its
// poll loop is stopped and the run forgotten.
func (b *runBroker) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	poll := sub.poll
	delete(poll.subscribers, sub)

	if len(poll.subscribers) > 0 {
		return
	}

	poll.stop()

	// Only forget this exact poll — never a fresh one a concurrent subscribe may
	// have installed for the same run under the same lock.
	if b.polls[poll.runID] == poll {
		delete(b.polls, poll.runID)
	}
}

// run is one run's shared poll loop: fetch the run detail every
// eventPollInterval and broadcast it to every current subscriber, until the run
// reaches a terminal status, the hosting server drains, or the last subscriber
// leaves (ctx cancelled by unsubscribe).
func (b *runBroker) run(ctx context.Context, poll *runPoll) {
	ticker := time.NewTicker(eventPollInterval)
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
				// A transient Temporal blip: log and retry next tick rather than
				// tearing down an otherwise-healthy shared stream over one RPC.
				slog.WarnContext(ctx, "runsapi: poll for run event stream failed", "run_id", poll.runID, "error", err)

				continue
			}

			terminal := detail.Status != runningStatus
			b.broadcast(poll, detail, terminal)

			if terminal {
				// Stop polling a finished run. The poll entry lingers (with the
				// terminal detail cached) until its subscribers disconnect, so a
				// late subscriber still gets a done frame.
				return
			}
		}
	}
}

// broadcast delivers detail to every current subscriber of poll, caching it as
// the terminal detail when the run has finished.
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

// sendLatest delivers detail on a size-1, latest-wins channel without ever
// blocking: if a prior update is still pending (a slow consumer), it is dropped
// in favour of this newer one. Only the broker (under b.mu) ever sends, so the
// drain-then-send is race-free.
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

// writeSSEEvent writes one Server-Sent Event named event with body
// JSON-encoded as its data, then flushes so it reaches the client
// immediately rather than sitting in a buffer until more data accumulates —
// an infrequent stream of updates is only useful if each one arrives
// promptly. It reports whether the write succeeded; a failure here almost
// always means the client disconnected (a write to a now-closed connection),
// which streamRunEvents treats the same as r.Context().Done() being
// signaled — stop, there is nothing left to do.
func writeSSEEvent(w http.ResponseWriter, responseController *http.ResponseController, event string, body interface{}) bool {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("runsapi: encode SSE event failed", "event", event, "error", err)

		return false
	}

	// Bound this write+flush with a fresh deadline: a TCP-backpressured client
	// must not block the handler goroutine indefinitely (which would keep it
	// off the drain-observing select and stall graceful shutdown — see
	// sseWriteTimeout). Reset per write, so it only ever spans one write, never
	// the idle wait between events. A ResponseController that does not support
	// deadlines returns ErrNotSupported here, which is fine to ignore — the
	// stream simply falls back to the server-wide timeout behavior.
	_ = responseController.SetWriteDeadline(time.Now().Add(sseWriteTimeout))

	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}

	if err := responseController.Flush(); err != nil {
		return false
	}

	return true
}

// writeSSEComment writes an SSE comment line (a line beginning with ":", which
// EventSource ignores) and flushes it, as a keepalive on an otherwise-idle
// stream (see sseHeartbeatInterval). Like writeSSEEvent it bounds the write with
// a fresh deadline and reports whether it succeeded; a failure means the client
// is gone and streamRunEvents should stop.
func writeSSEComment(w http.ResponseWriter, responseController *http.ResponseController) bool {
	_ = responseController.SetWriteDeadline(time.Now().Add(sseWriteTimeout))

	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return false
	}

	return responseController.Flush() == nil
}
