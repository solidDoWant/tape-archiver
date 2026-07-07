package backup

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// recordingWorker is a worker.Worker that records what is registered on it
// without standing up a real Temporal worker. It embeds the interface so it
// satisfies worker.Worker; only the registration methods the backup package
// calls are implemented — any other call would panic, flagging an unexpected
// dependency.
type recordingWorker struct {
	worker.Worker

	workflows  []recordedWorkflow
	activities []any
}

type recordedWorkflow struct {
	fn   any
	name string
}

func (r *recordingWorker) RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions) {
	r.workflows = append(r.workflows, recordedWorkflow{fn: w, name: options.Name})
}

func (r *recordingWorker) RegisterActivity(a any) {
	r.activities = append(r.activities, a)
}

func TestRegisterControl(t *testing.T) {
	t.Parallel()

	rw := &recordingWorker{}

	RegisterControl(rw, ControlConfig{FailureWebhookURL: "https://discord.example/webhook"})

	// The control worker registers the Backup workflow (under the contract's
	// WorkflowType so clients can start it by name) and the Eject-pause
	// auto-resume child workflow (issue #168), which keeps the run's history
	// bounded across long pauses by ContinueAsNew'ing its own poll loop.
	require.Len(t, rw.workflows, 2)

	backupWorkflow := findWorkflow(t, rw.workflows, WorkflowType)
	assert.Equal(t,
		reflect.ValueOf(Backup).Pointer(),
		reflect.ValueOf(backupWorkflow.fn).Pointer(),
		"the workflow registered under WorkflowType must be Backup",
	)

	ioWaitWorkflow := findWorkflow(t, rw.workflows, IOStationWaitWorkflowType)
	assert.Equal(t,
		reflect.ValueOf(ioStationWaitWorkflow).Pointer(),
		reflect.ValueOf(ioWaitWorkflow.fn).Pointer(),
		"the workflow registered under IOStationWaitWorkflowType must be ioStationWaitWorkflow",
	)

	// The failure-alert activity is registered wired with the configured URL.
	failureActivities := findFailureActivities(t, rw.activities)
	assert.Equal(t, "https://discord.example/webhook", failureActivities.WebhookURL)

	// The control-side Resolve activity is registered (k8s resolution runs on the
	// control worker, SPEC §16).
	assert.True(t, hasActivity[*ResolveControlActivities](rw.activities),
		"the control worker must register the Resolve control activity")

	// The Pack activity is registered: bin-packing is pure planning and runs on
	// the control worker (SPEC §4.1, §4.3 phase 3).
	assert.True(t, hasActivity[*PackActivities](rw.activities),
		"the control worker must register the Pack activity")
}

func TestRegisterData(t *testing.T) {
	t.Parallel()

	rw := &recordingWorker{}

	RegisterData(rw, DataConfig{
		StagingDir:          "/mnt/bulk-pool-01/archive/.tape-staging",
		RecoveryBinariesDir: "/opt/recovery-bin",
		RecoverySourcesDir:  "/opt/recovery-src",
	})

	// The data worker hosts no workflow; it only registers the bulk-data phase
	// activities: Resolve data, Prepare, Generate PAR2, Verify, Load, Write
	// (WriteActivities + TeardownActivities sharing a registry), WriteHealth,
	// Eject, Report, Deliver, and the per-disc optical Burn/Verify.
	assert.Empty(t, rw.workflows)
	assert.Len(t, rw.activities, 12)
	assert.True(t, hasActivity[*ResolveDataActivities](rw.activities),
		"the data worker must register the Resolve data activity")
	assert.True(t, hasActivity[*PrepareActivities](rw.activities),
		"the data worker must register the Prepare activity")
	assert.True(t, hasActivity[*GeneratePAR2Activities](rw.activities),
		"the data worker must register the Generate PAR2 activity")
	assert.True(t, hasActivity[*VerifyActivities](rw.activities),
		"the data worker must register the Verify activity")
	assert.True(t, hasActivity[*LoadActivities](rw.activities),
		"the data worker must register the Load activity")
	assert.True(t, hasActivity[*WriteActivities](rw.activities),
		"the data worker must register the Write activities (FormatTape, WriteTree, FinalizeTape)")
	assert.True(t, hasActivity[*TeardownActivities](rw.activities),
		"the data worker must register the TeardownSession activity")
	assert.True(t, hasActivity[*WriteHealthActivities](rw.activities),
		"the data worker must register the WriteHealth activity (MeasureWriteHealth)")
	assert.True(t, hasActivity[*EjectActivities](rw.activities),
		"the data worker must register the Eject activity")
	assert.True(t, hasActivity[*ReportActivities](rw.activities),
		"the data worker must register the Report activity (report/ISO build)")
	assert.True(t, hasActivity[*DeliverActivities](rw.activities),
		"the data worker must register the Deliver activity (Discord delivery)")
	assert.True(t, hasActivity[*BurnActivities](rw.activities),
		"the data worker must register the optical Burn/Verify activities (BurnDisc, VerifyDisc)")
}

// findWorkflow returns the recorded workflow registered under the given name,
// failing the test if none matches.
func findWorkflow(t *testing.T, workflows []recordedWorkflow, name string) recordedWorkflow {
	t.Helper()

	for _, wf := range workflows {
		if wf.name == name {
			return wf
		}
	}

	t.Fatalf("no workflow registered under name %q", name)

	return recordedWorkflow{}
}

// hasActivity reports whether any registered activity is of type T.
func hasActivity[T any](activities []any) bool {
	for _, activity := range activities {
		if _, ok := activity.(T); ok {
			return true
		}
	}

	return false
}

// findFailureActivities returns the single *FailureActivities among the
// registered activities, failing the test if it is absent.
func findFailureActivities(t *testing.T, activities []any) *FailureActivities {
	t.Helper()

	for _, activity := range activities {
		if failureActivities, ok := activity.(*FailureActivities); ok {
			return failureActivities
		}
	}

	t.Fatal("FailureActivities was not registered on the control worker")

	return nil
}
