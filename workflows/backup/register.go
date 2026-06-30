package backup

import (
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// ControlConfig holds the control worker's settings that the backup workflow's
// control-side registrations need. It is the seam through which cmd/worker
// passes operational configuration (parsed from the environment) into the
// workflow package.
type ControlConfig struct {
	// FailureWebhookURL is the Discord failure webhook (DISCORD_FAILURE_WEBHOOK_URL).
	// Empty disables failure alerting (SPEC §11).
	FailureWebhookURL string
	// K8sDatasetParent is democratic-csi's datasetParentName, used to rebuild
	// absolute ZFS snapshot paths during k8s resolution (SPEC §3, §16). Empty
	// treats CSI snapshotHandles as already absolute.
	K8sDatasetParent string
}

// DataConfig holds the data worker's settings that the backup workflow's
// data-side registrations need. It is the seam through which cmd/worker passes
// operational configuration (parsed from the environment) into the workflow
// package, mirroring ControlConfig.
type DataConfig struct {
	// StagingDir is the directory the Prepare phase stages prepared archives
	// into (TAPE_STAGING_DIR), a subdirectory of an existing dataset on the
	// storage host (SPEC §4.1). Required; the Prepare activity fails when empty.
	StagingDir string
}

// RegisterControl registers everything the control worker hosts: the Backup
// workflow under WorkflowType (so clients can start it by the contract name),
// the operational failure-alert activity wired with the configured webhook URL,
// and the control-side phase activities. It is called from cmd/worker for the
// control role.
//
// The phase activities are stubs in this scaffold; each control-side phase
// sub-issue replaces its stub here.
func RegisterControl(w worker.Worker, cfg ControlConfig) {
	w.RegisterWorkflowWithOptions(Backup, workflow.RegisterOptions{Name: WorkflowType})

	w.RegisterActivity(&FailureActivities{WebhookURL: cfg.FailureWebhookURL})

	w.RegisterActivity(newResolveControlActivities(cfg.K8sDatasetParent))
	w.RegisterActivity(newPackActivities())
	w.RegisterActivity(reportActivity)
	w.RegisterActivity(deliverActivity)
}

// RegisterData registers the data worker's bulk-data phase activities
// (SPEC §4.1), wired with the data worker's operational configuration. It is
// called from cmd/worker for the data role.
func RegisterData(w worker.Worker, cfg DataConfig) {
	w.RegisterActivity(newResolveDataActivities())
	w.RegisterActivity(newPrepareActivities(cfg.StagingDir))
	w.RegisterActivity(newGeneratePAR2Activities())
	w.RegisterActivity(newVerifyActivities())
	w.RegisterActivity(newLoadActivities())

	// FormatTape, WriteTree, and FinalizeTape share a registry so a live
	// mount parked by WriteTree survives into FinalizeTape across the activity
	// boundary (sessions pin both to the same process).
	registry := newMountRegistry()
	w.RegisterActivity(newWriteActivities(registry, cfg.StagingDir))
	w.RegisterActivity(newTeardownActivities(registry))

	w.RegisterActivity(newEjectActivities())
}
