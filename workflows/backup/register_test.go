package backup

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// recordingWorker records registrations; it embeds worker.Worker so any other
// call panics, flagging an unexpected dependency.
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

	// Backup plus the Eject-pause child workflow (#168).
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

	failureActivities := findFailureActivities(t, rw.activities)
	assert.Equal(t, "https://discord.example/webhook", failureActivities.WebhookURL)

	assert.True(t, hasActivity[*ResolveControlActivities](rw.activities),
		"the control worker must register the Resolve control activity")

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

	assert.Empty(t, rw.workflows)
	assert.Len(t, rw.activities, 16)
	assert.True(t, hasActivity[*ResolveDataActivities](rw.activities),
		"the data worker must register the Resolve data activity")
	assert.True(t, hasActivity[*RunSnapshotActivities](rw.activities),
		"the data worker must register the run-scoped snapshot activities")
	assert.True(t, hasActivity[*HoldActivities](rw.activities),
		"the data worker must register the source-snapshot Hold activity")
	assert.True(t, hasActivity[*PrepareActivities](rw.activities),
		"the data worker must register the Prepare activity")
	assert.True(t, hasActivity[*GeneratePAR2Activities](rw.activities),
		"the data worker must register the Generate PAR2 activity")
	assert.True(t, hasActivity[*VerifyActivities](rw.activities),
		"the data worker must register the Verify activity")
	assert.True(t, hasActivity[*MirrorActivities](rw.activities),
		"the data worker must register the MirrorLibrary activity so a dry run can "+
			"mirror the real library onto the virtual one")
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
	assert.True(t, hasActivity[*CleanupActivities](rw.activities),
		"the data worker must register the staging Cleanup activity")
}

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

func hasActivity[T any](activities []any) bool {
	for _, activity := range activities {
		if _, ok := activity.(T); ok {
			return true
		}
	}

	return false
}

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
