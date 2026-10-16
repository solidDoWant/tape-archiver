package backup

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// ControlConfig is the control worker's operational configuration, parsed from
// the environment by cmd/worker.
type ControlConfig struct {
	// FailureWebhookURL is DISCORD_FAILURE_WEBHOOK_URL; empty disables failure
	// alerting (SPEC §11).
	FailureWebhookURL string
	// K8sDatasetParent is democratic-csi's datasetParentName, used to rebuild
	// absolute ZFS snapshot paths (SPEC §3, §16). Empty treats snapshotHandles as
	// absolute.
	K8sDatasetParent string
}

// DataConfig is the data worker's operational configuration, parsed from the
// environment by cmd/worker.
type DataConfig struct {
	// StagingDir is TAPE_STAGING_DIR (SPEC §4.1). Required.
	StagingDir string
	// RecoveryBinariesDir holds the statically linked recovery binaries for the ISO
	// (TAPE_RECOVERY_BINARIES_DIR, SPEC §10). Required.
	RecoveryBinariesDir string
	// RecoverySourcesDir holds the recovery tools' source archives for the ISO's
	// src/ (TAPE_RECOVERY_SOURCES_DIR, SPEC §2, §10). Required.
	RecoverySourcesDir string
	// MetricsRegisterer receives the write-health gauges (SPEC §14); nil disables
	// them (the report still records them).
	MetricsRegisterer prometheus.Registerer
}

// RegisterControl registers the Backup workflow and the control-side
// activities on w.
func RegisterControl(w worker.Worker, cfg ControlConfig) {
	w.RegisterWorkflowWithOptions(Backup, workflow.RegisterOptions{Name: WorkflowType})

	// Self-continuing child workflow for the Eject-pause poll loop, keeping the
	// run's history bounded (#168).
	w.RegisterWorkflowWithOptions(ioStationWaitWorkflow, workflow.RegisterOptions{Name: IOStationWaitWorkflowType})

	w.RegisterActivity(&FailureActivities{WebhookURL: cfg.FailureWebhookURL})

	w.RegisterActivity(newResolveControlActivities(cfg.K8sDatasetParent))
	w.RegisterActivity(newPackActivities())
}

// RegisterData registers the bulk-data activities (SPEC §4.1) on w.
func RegisterData(w worker.Worker, cfg DataConfig) {
	w.RegisterActivity(newResolveDataActivities(cfg.StagingDir))
	w.RegisterActivity(newRunSnapshotActivities())
	w.RegisterActivity(newHoldActivities())
	w.RegisterActivity(newPrepareActivities(cfg.StagingDir))
	w.RegisterActivity(newGeneratePAR2Activities())
	w.RegisterActivity(newVerifyActivities())
	// Dry runs only (Library.MirrorSourceChanger).
	w.RegisterActivity(newMirrorActivities())
	w.RegisterActivity(newLoadActivities())

	// Shared so a mount parked by WriteTree survives into FinalizeTape (sessions
	// pin both to the same process).
	registry := newMountRegistry()
	w.RegisterActivity(newWriteActivities(registry, cfg.StagingDir))
	w.RegisterActivity(newTeardownActivities(registry))

	w.RegisterActivity(newWriteHealthActivities(cfg.MetricsRegisterer))

	w.RegisterActivity(newEjectActivities())

	// Report and Deliver run here: they need the staged files and recovery
	// binaries, and it keeps the ISO off the Temporal payload path.
	w.RegisterActivity(newReportActivities(cfg.StagingDir, cfg.RecoveryBinariesDir, cfg.RecoverySourcesDir))
	w.RegisterActivity(newDeliverActivities())

	w.RegisterActivity(newCleanupActivities(cfg.StagingDir))

	// Burners are attached to the storage host; the activities are stateless.
	w.RegisterActivity(newBurnActivities())
}
