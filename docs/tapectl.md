# tapectl — operator CLI

`tapectl` submits a run config to Temporal as a backup workflow, triggers dry-runs
against the `mhvtl` virtual library, inspects the status of running or completed runs,
and resumes, aborts, or cancels an in-progress run. It is built into `bin/tapectl` by
`make build`.

The run config it submits is documented in [configuration.md](configuration.md); the
workflow phases it reports are defined in `SPEC.md` §4.3.

---

## Connection

`tapectl` connects to Temporal using the same environment variables as the worker,
loaded via the Temporal `envconfig` package:

| Variable | Required | Description |
|----------|----------|-------------|
| `TEMPORAL_ADDRESS` | yes | Frontend gRPC address, e.g. `localhost:7233`. Every subcommand exits with a descriptive error if it is unset. |
| `TEMPORAL_NAMESPACE` | no | Namespace to use (defaults to the envconfig default). |

Other `TEMPORAL_*` variables (TLS, API key, etc.) are honored as documented by the
Temporal envconfig loader.

---

## `tapectl run`

Submit a run config as a backup workflow and print the resulting workflow ID.

```
tapectl run --config <file> [--dry-run]
```

| Flag | Required | Description |
|------|----------|-------------|
| `--config` | yes | Path to the run-config JSON file. |
| `--dry-run` | no | Override the library device targets to the `mhvtl` virtual library (see below). |

The config is fully validated **client-side before Temporal is contacted**: malformed
JSON, an unrecognized field, or any validation failure exits non-zero with a
human-readable error and submits nothing. On success the workflow ID is printed on its
own line:

```
$ tapectl run --config run.json
backup
```

### One run at a time

Every run submits under the fixed workflow ID `backup`. Backup runs are **mutually
exclusive**: the model is serial — one data worker on one storage host stages every
tape to disk before any write (SPEC §4.2, §4.3) — so two runs must never execute
concurrently. If a run is submitted while another is still in progress, the submission
is refused with an error identifying the in-progress run rather than racing it on the
shared library:

```
$ tapectl run --config run.json
a backup run is already in progress (workflow ID "backup", run ID 018f…);
backup runs are mutually exclusive — wait for it to finish or inspect it with `tapectl status`
```

Once the in-progress run has finished (success or failure), a new run submitted under
the same ID starts normally. There is no flag to submit under a different ID — the
singleton ID is what makes the guard airtight.

### Dry-run

`--dry-run` rewrites the config's `library.changer` and `library.drives` to point at the
`mhvtl` virtual tape library instead of the real changer and drives, so the same code
path exercises virtual hardware end to end (SPEC §12). The blank slots are left
untouched. The override is applied client-side before submission, and the result is
re-validated.

The `mhvtl` device nodes must be named explicitly through the environment — there is
**no hardware default**. The nodes `mhvtl` typically presents (`/dev/sch0`, `/dev/nstX`)
are byte-identical to the real library, and because these variables are read on the
`tapectl` client host while the devices are opened by the worker on the storage host,
the submitted config carries no dry-run marker the worker could honor. A silent default
would therefore let `tapectl run --dry-run`, launched from a host without these
variables, aim a "test" run at the real changer and drives. To close that hole, all
three variables are **required** for `--dry-run`; if any is unset the command **fails
fast** naming the missing variable(s) and submits nothing. The dev shell's `mhvtl-up`
exports them.

| Variable | Required for `--dry-run` | Description |
|----------|--------------------------|-------------|
| `MHVTL_CHANGER_DEV` | yes | `mhvtl` media changer node (e.g. `/dev/sch0`). |
| `MHVTL_DRIVE0_DEV` | yes | First `mhvtl` tape drive node (e.g. `/dev/nst0`). |
| `MHVTL_DRIVE1_DEV` | yes | Second `mhvtl` tape drive node (e.g. `/dev/nst1`). |

#### Library mirroring under `--dry-run`

A dry run is only a rehearsal if it meets the conditions the real run would. So, alongside
the device redirect, `--dry-run` records the changer the run *would* have targeted in
`library.mirrorSourceChanger` — the one point where both the real and virtual devices are
known. Before the tape path runs, the data worker reads that real library's slot inventory
and rewrites the virtual library to match: every slot occupied there is occupied here under
the same barcode, and every empty slot stays empty.

Without this the two libraries drift apart, and the drift hides exactly the problems a dry
run exists to find: a virtual library stocked with a full complement of blanks passes a
rehearsal that the real library — short of blanks, or with tapes left in drives by an
earlier run — would fail. Expect a mirrored dry run to be **stricter** than an unmirrored
one for this reason.

The mirror only ever *reads* the real library (`READ ELEMENT STATUS`). It rearranges the
virtual library while its daemons keep running: drives are unloaded, unwanted tapes leave
through the virtual I/O station, the mirrored volumes are recreated blank, and missing
barcodes are inserted through the I/O station and moved to their slots. The `mhvtl` daemons
are never restarted or signalled. The mirror then checks the virtual library reports exactly
the mirrored slots with empty drives and I/O station, and fails naming anything that differs.
It is skipped entirely on a real run: submission discards any client-supplied
`library.mirrorSourceChanger` (a dry run recomputes it), and the worker ignores the field on
any run not recorded as a dry run. It is also skipped when the field is empty, as on a dev
deployment whose configured library already *is* the `mhvtl` one (there is nothing to mirror
from).

Media are recreated **blank**: element status reports which slots hold a tape but not
whether it has data. Slot occupancy is the property that decides whether a run can find its
configured blanks, which is the failure this exists to surface.

The data worker needs the `mhvtl` device nodes, `/etc/mhvtl` and the media home
bind-mounted, and the host IPC namespace (`--ipc=host`) to reach the daemons' message
queue; see the reference unit in `deploy/data-worker/`. The `mhvtl` tooling is bundled in
the data-worker image, so nothing `mhvtl`-related need be installed on the host.
The media home is read from `MHVTL_HOME_PATH` in `/etc/mhvtl/mhvtl.conf` — the same place
`mhvtl`'s own tooling reads it — and must be bind-mounted at that same path inside the
container. Nothing is compiled in, so a host is free to keep its virtual volumes wherever
suits it (this project's storage host puts them on the bulk pool, since a dry run writes
real volumes and would otherwise fill the root disk). A media home that `mhvtl.conf` does
not declare, or that is not mounted, **fails the mirror outright** rather than being
created: media written somewhere `vtllibrary` is not looking would produce a mirror that
reports success and a run that dies later at Load.

For the same reason the mirror checks its own work: after regenerating the media it
confirms every mirrored barcode has a volume under the media home, and fails naming the
ones that do not. `make_vtl_media` exits 0 whether or not it created anything, and
`vtllibrary` derives slot occupancy from `library_contents` alone — so without this check a
library reports tapes that cannot be mounted.

#### Optical burning under `--dry-run`

`--dry-run` **disables optical burning**: it removes the `delivery.opticalBurn` section
from the submitted config so the burn phase is a no-op. `mhvtl` provides no virtual
optical burner, so — unlike the tape library — there is no safe device to redirect to,
and the submitted config carries no dry-run marker the worker could honor. Were the
section left in place, the worker would probe, pause on, blank, and irreversibly burn the
real burner named in `delivery.opticalBurn.drives` (including write-once M-DISC media)
during what is meant to be a hardware-free test. With the section neutralized, a dry-run
**never probes, blanks, pauses on, or writes any device in `delivery.opticalBurn.drives`**,
while the tape path is still exercised end to end against `mhvtl` — so a production config
that also configures burning remains dry-runnable for its tape path. When a dry-run
disables a configured burn section, `tapectl` prints a one-line advisory to stderr so the
skipped burning is not silent. Optical burning is only ever exercised by a real
(non-`--dry-run`) run against real hardware.

---

## `tapectl status`

Print the backup run's current execution status, its last completed phase, and the phase it is currently running. Runs are a singleton (SPEC §4.2), so it takes no arguments.

```
tapectl status
```

```
$ tapectl status
Workflow:             backup
Status:               Running
Last completed phase: Resolve
Current phase:        Prepare (running since 2026-10-01T12:03:00Z)
```

The last completed phase is read via a Temporal query answered by the running workflow. A query needs a polling control worker, which KEDA scales to zero during long data-queue activities such as Prepare and Write, so when the query fails the phase is derived from the run's workflow history instead — the same derivation the web UI's phase timeline uses. It is reported as `unavailable` only when the history cannot be read either; the execution status is still shown. Before any phase has completed it is reported as `none`.

The current phase is always derived from workflow history and matches the web UI's active phase(s), each with the time its first activity was scheduled (UTC). The interleaved Load/Write/Eject path can list more than one, comma-separated. It is `none` once the run has closed (or before any activity is scheduled) and `unavailable` when the history cannot be read.

---

## `tapectl resume`

Resume the paused backup run. It resumes any of the operator-in-the-loop pauses (SPEC §4.3):
the Eject phase paused because the import/export station filled (phase 8), the tape path
paused because a Load or Write failed for one drive-set (phases 6–8), or the Burn phase
paused to load fresh blank discs or on a burn failure/verify mismatch/refused non-blank disc
(phase 10). Runs are a singleton (SPEC §4.2), so it takes no arguments.

```
tapectl resume
```

```
$ tapectl resume
Resume signal sent to run backup.
```

**Eject pause.** When a run writes more physical tapes than the library has I/O slots, the
Eject phase exports as many as fit, alerts the operator (on the failure webhook) which
tapes to remove, and pauses. After physically removing the exported tapes and clearing the
station, run `tapectl resume` to continue: the run re-reads the changer inventory and
exports the remaining tapes into the freed slots. Libraries that report the import/export
access bit resume **automatically** once the station is cleared and closed, without this
command; `resume` is the fallback for libraries that do not. If no one responds within
`library.ioWaitTimeoutSeconds` (default 12 hours), the run fails with every written tape
left in an I/O or storage slot.

**Load/Write-failure pause.** When a Load or Write fails for one drive-set, the run keeps
the tapes that wrote successfully, ejects the failed tapes, and pauses — alerting the
operator (on the failure webhook) which tapes failed and which storage slots to restock
with fresh blanks. After loading fresh blank tapes into those slots, run `tapectl resume`:
the run re-drives **only** the failed tapes onto the fresh blanks, never re-formatting a
tape already written. If no one responds within `library.writeFailureWaitTimeoutSeconds`
(default 12 hours), the run fails in that defined paused state. To end the run instead of
resuming, use [`tapectl abort`](#tapectl-abort).

Sending `resume` to a run that is not paused has no effect.

---

## `tapectl abort`

Abort the backup run paused because a Load or Write failed for one drive-set (SPEC §4.3
phases 6–8): instead of swapping in fresh blanks and resuming, end the run in a defined,
reported state with no further tapes written. Runs are a singleton (SPEC §4.2), so it takes
no arguments.

```
tapectl abort
```

```
$ tapectl abort
Abort signal sent to run backup.
```

The tapes that wrote successfully before the failure are already ejected and recorded; the
report covers them (as does the recovery ISO, when optical burning is enabled). Sending
`abort` during a Burn-phase pause also acts: it ends the run with no further discs burned
(the discs that already verified are kept). Outside a Load/Write pause or a Burn-phase
pause, `abort` has no effect.

---

## `tapectl cancel`

Cancel the in-progress backup run, whether or not it is paused. Unlike `abort` — which
sends the operator-abort signal and only ends a run already paused on a Load/Write (or
Burn-phase) failure — `cancel` stops *any* running backup: it requests graceful Temporal
cancellation (`CancelWorkflow`), the same action the web UI's **Cancel run** button takes.
Runs are a singleton (SPEC §4.2), so it takes no arguments.

```
tapectl cancel
```

```
$ tapectl cancel
Cancellation requested for run backup.
```

Cancellation is graceful, never a forceful terminate: it is delivered into the workflow,
whose deferred cleanup runs on a disconnected context (SPEC §10), so the run tears down its
LTFS mounts, releases its ZFS hold, posts the same failure/cancellation alert to the Discord
failure webhook as any other run end, and closes in the `Canceled` state rather than being
killed mid-flight and left with wedged mounts. The command returns as soon as the request is
accepted; watch [`tapectl status`](#tapectl-status) (or the web UI) for the run to actually
close. Cancelling when there is no in-progress run reports the error Temporal returns.
