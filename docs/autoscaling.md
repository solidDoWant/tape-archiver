# Autoscaling the control worker (scale-to-zero)

The **control worker** (SPEC §4.1) is a lightweight Temporal poller on the `control` task
queue. It normally runs as a fixed-replica `Deployment` 24/7, even though a backup run
fires only once every few months. This page records the prerequisites and the sizing
decision for making it **optionally scale-to-zero** — a KEDA `ScaledJob` that spawns a
worker on demand when a run lands on the `control` queue, plus a worker-side idle-exit
(`WORKER_IDLE_EXIT_AFTER`) that drains and exits when the queue is idle. Autoscaling is
strictly **opt-in**; the default chart render stays a fixed-replica `Deployment`.

This is the confirmed outcome of the prerequisite spike (issue #114). Implementation lands
in the downstream sub-issues of #113 (worker idle-exit, chart `ScaledJob` path, e2e
coverage); this page is the single source those cite.

## Prerequisites (confirmed)

| Component | Minimum | Why | Deployed (verified 2026-07-06) |
|-----------|---------|-----|--------------------------------|
| Temporal server | 1.24 | `DescribeTaskQueue` populates `ApproximateBacklogCount`, the metric the KEDA Temporal scaler reads to decide `0 → 1`. | **1.31.0** — production (`temporalio/server:1.31.0`, `data` namespace) **and** the `make temporal-up` dev stack (`temporalio/temporal:1.7.0`, embedded server 1.31.0). |
| KEDA | 2.16 | The `temporal` scaler is bundled in KEDA from 2.16 onward. | **2.19.0** — already installed cluster-wide (`ghcr.io/kedacore/keda:2.19.0`, `system-controllers` namespace). media-processor already drives it (`media-temporal-keda-auth`). |

`ApproximateBacklogCount` was confirmed live: `temporal task-queue describe --task-queue
<queue> --task-queue-type workflow -o json` returns a populated `approximateBacklogCount`
on both the dev stack and the production server. No Temporal upgrade and no KEDA install are
required — both prerequisites are already satisfied in the target environment.

## Replay cost on respawn (measured)

Because the tape-write window is *hours* and operator pauses can be *days*, the control
worker **will** exit and respawn mid-run: while a long data-worker activity (e.g. writing a
tape) is in flight, the control worker has no in-flight `control` task and hits its idle
window. On respawn it replays the full workflow history. That replay re-executes only the
deterministic workflow function over the recorded events — **activities are never re-run** —
so the cost is pure workflow-side CPU, independent of tape/ZFS/PAR2 I/O.

Measured on the dev Temporal (server 1.31.0) by running the real backup workflow (control +
data workers against mhvtl + ZFS), exporting history with `temporal workflow show -o json`,
and replaying it 300× with `worker.WorkflowReplayer`:

| Run | Physical tapes | History events | History bytes | Replay p50 | Replay p95 |
|-----|---------------:|---------------:|--------------:|-----------:|-----------:|
| 1 copy  | 1 | 110 | ~214 KB | 4.0 ms | 8.5 ms |
| 3 copies | 3 | 224 | ~447 KB | 5.9 ms | 8.3 ms |

Each physical tape adds a fixed write loop of **9 activities** (Load, session-create,
FormatTape, WriteTree, FinalizeTape, MeasureWriteHealth, session-teardown,
session-complete, Eject) ≈ **+57 events / +116 KB / +~0.9 ms replay**; the run prologue and
epilogue (Resolve, Prepare, Pack, Generate PAR2, Verify, Report, Deliver) are a one-time
cost. This gives a linear model:

- events ≈ 53 + 57 · (physical tapes)
- full replay ≈ 2.3 ms + ~16 µs/event ≈ 2.3 ms + ~0.9 ms · (physical tapes)

An operator pause/resume adds only a signal plus a handful of events — negligible. A
realistic tens-of-tapes run replays in tens of milliseconds. Even at Temporal's soft
single-history ceiling (~50 K events / ~50 MB, roughly 877 physical tapes at 57 events per
tape), a full replay stays **under ~1 second** — and any larger run would
`ContinueAsNew` before reaching it.

### Replay-cost bound

Full-history replay on control-worker respawn stays **under 1 second** for any run Temporal
permits in a single history. Replay is therefore negligible against the hours-long tape
write window and multi-day operator pauses, and it does **not** constrain the idle-exit
window. This is the bound the parent #113 replay-cost acceptance criterion refers to.

## Idle-exit default

`WORKER_IDLE_EXIT_AFTER` defaults to **`15m`**.

Because replay is cheap, the idle window is not a replay tradeoff — it balances respawn
churn against idle cluster footprint. 15 minutes (media-processor's proven reference for a
workflow-bearing worker) avoids needless exit/respawn during the quick, back-to-back
control-phase activities and brief inter-activity gaps, while still scaling to zero during
the hours-long waits for data-worker activities and during multi-day operator pauses. The
value is a single static default; it is not auto-tuned. A respawn costs a KEDA poll plus
pod start (seconds), which is immaterial against the run's timescale, so operators may lower
it without a correctness concern.

## Status

The scale-to-zero feature is **viable in the target environment**: both prerequisites are
already met (Temporal 1.31.0, KEDA 2.19.0), replay cost is bounded well under a second, and
the idle-exit default is agreed at 15m. Implementation proceeds in the #113 sub-issues.
