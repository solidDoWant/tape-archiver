# tapectl — operator CLI

`tapectl` submits a run config to Temporal as a backup workflow, triggers dry-runs
against the `mhvtl` virtual library, and inspects the status of running or completed
runs. It is built into `bin/tapectl` by `make build`.

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
tapectl run --config <file> [--dry-run] [--id <id>]
```

| Flag | Required | Description |
|------|----------|-------------|
| `--config` | yes | Path to the run-config JSON file. |
| `--dry-run` | no | Override the library device targets to the `mhvtl` virtual library (see below). |
| `--id` | no | Workflow ID to submit under. Defaults to `backup-<timestamp>` (UTC, e.g. `backup-20260629T134505Z`). |

The config is fully validated **client-side before Temporal is contacted**: malformed
JSON, an unrecognized field, or any validation failure exits non-zero with a
human-readable error and submits nothing. On success the workflow ID is printed on its
own line:

```
$ tapectl run --config run.json
backup-20260629T134505Z
```

### Dry-run

`--dry-run` rewrites the config's `library.changer` and `library.drives` to point at the
`mhvtl` virtual tape library instead of the real changer and drives, so the same code
path exercises virtual hardware end to end (SPEC §12). The blank slots are left
untouched. The override is applied client-side before submission, and the result is
re-validated.

The device paths default to the values `mhvtl` presents in the dev/CI environment and
can be overridden via the environment — set these on a host where the real library
already occupies those nodes so a dry-run never targets real hardware:

| Variable | Default |
|----------|---------|
| `MHVTL_CHANGER_DEV` | `/dev/sch0` |
| `MHVTL_DRIVE0_DEV` | `/dev/nst0` |
| `MHVTL_DRIVE1_DEV` | `/dev/nst1` |

---

## `tapectl status`

Print a workflow's current execution status and its last completed phase.

```
tapectl status <workflow-id>
```

```
$ tapectl status backup-20260629T134505Z
Workflow:             backup-20260629T134505Z
Status:               Running
Last completed phase: Verify
```

The last completed phase is read via a Temporal query answered by the running workflow.
If no worker is currently polling the workflow (e.g. a completed run with no live
worker), or the workflow does not yet answer the query, the phase is reported as
`unavailable` while the execution status is still shown. Before any phase has completed
it is reported as `none`.
