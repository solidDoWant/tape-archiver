//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// requireTemporalAddress skips the test unless a Temporal server is reachable
// at TEMPORAL_ADDRESS. The make test-integration target arranges this via
// temporal-up before invoking the suite.
func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig points TEMPORAL_CONFIG_FILE at a fresh empty TOML file
// so envconfig does not pick up a stray ~/.config/temporalio/temporal.toml on
// the host, mirroring pkg/temporalclient's integration tests.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

// TestRunRegistersWorkerOnTaskQueue starts the worker for each role and asserts
// that a poller appears on the matching task queue — the observable behavior
// behind "the Temporal UI shows the worker as registered on the correct task
// queue". Disabling the metrics endpoint keeps the two role workers from
// racing for the same listen address.
func TestRunRegistersWorkerOnTaskQueue(t *testing.T) {
	tests := []struct {
		name  string
		role  Role
		queue string
	}{
		{name: "control role polls control queue", role: RoleControl, queue: controlTaskQueue},
		{name: "data role polls data queue", role: RoleData, queue: dataTaskQueue},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireTemporalAddress(t)
			isolateTemporalConfig(t)

			t.Setenv("ROLE", string(test.role))
			t.Setenv("METRICS_ADDR", "")
			// Disable the health server too, so sequential subtests do not
			// contend for the default health port.
			t.Setenv("HEALTH_ADDR", "")

			// interruptCh stands in for worker.InterruptCh: closing it drives
			// the same run-phase drain a SIGTERM would, without signalling the
			// test process.
			interruptCh := make(chan interface{})
			runErr := make(chan error, 1)

			go func() { runErr <- run(t.Context(), interruptCh) }()

			// A separate client probes the task queue the worker should be
			// polling. New performs its own startup health check.
			probe, shutdown, err := temporalclient.New(t.Context(), nil)
			require.NoError(t, err)

			defer shutdown()

			require.Eventually(t, func() bool {
				resp, err := probe.DescribeTaskQueue(t.Context(), test.queue, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
				if err != nil {
					return false
				}

				return len(resp.GetPollers()) > 0
			}, 30*time.Second, 250*time.Millisecond, "worker did not register a poller on the %q task queue", test.queue)

			// Trigger graceful drain and confirm run returns without error.
			close(interruptCh)

			select {
			case err := <-runErr:
				assert.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("run did not return after interrupt")
			}
		})
	}
}
