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

	// The Backup workflow is registered under the contract's WorkflowType so
	// clients can start it by name.
	require.Len(t, rw.workflows, 1)
	assert.Equal(t, WorkflowType, rw.workflows[0].name)
	assert.Equal(t,
		reflect.ValueOf(Backup).Pointer(),
		reflect.ValueOf(rw.workflows[0].fn).Pointer(),
		"the registered workflow must be Backup",
	)

	// The failure-alert activity is registered wired with the configured URL.
	failureActivities := findFailureActivities(t, rw.activities)
	assert.Equal(t, "https://discord.example/webhook", failureActivities.WebhookURL)
}

func TestRegisterData(t *testing.T) {
	t.Parallel()

	rw := &recordingWorker{}

	RegisterData(rw)

	// The data worker hosts no workflow; it only registers the bulk-data phase
	// activities.
	assert.Empty(t, rw.workflows)
	assert.Len(t, rw.activities, 6)
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
