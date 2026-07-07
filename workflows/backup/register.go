package backup

import (
	"github.com/prometheus/client_golang/prometheus"
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
	// RecoveryBinariesDir is the directory holding the statically linked recovery
	// binaries (age, par2, zstd, tar) the Report phase stages into the recovery
	// ISO (TAPE_RECOVERY_BINARIES_DIR, SPEC §10). Required; the Report activity
	// fails when empty or when a binary is not statically linked.
	RecoveryBinariesDir string
	// RecoverySourcesDir is the directory holding the recovery tools' upstream
	// source archives the Report phase stages into the recovery ISO's src/
	// (TAPE_RECOVERY_SOURCES_DIR, SPEC §2, §10). Required; the Report activity fails
	// when empty or when it yields no source archives.
	RecoverySourcesDir string
	// MetricsRegisterer is the Prometheus registry the data worker's write-health
	// gauges register against (SPEC §14). It is the same registry that backs the
	// worker's /metrics endpoint; nil when metrics are disabled, in which case
	// write-health metrics are simply not exported (the report still records them).
	MetricsRegisterer prometheus.Registerer
}

// RegisterControl registers everything the control worker hosts: the Backup
// workflow under WorkflowType (so clients can start it by the contract name),
// the operational failure-alert activity wired with the configured webhook URL,
// and the control-side planning activities (snapshot resolution, bin-packing).
// It is called from cmd/worker for the control role. The bulk-data phases —
// including report/ISO building and delivery — run on the data worker.
func RegisterControl(w worker.Worker, cfg ControlConfig) {
	w.RegisterWorkflowWithOptions(Backup, workflow.RegisterOptions{Name: WorkflowType})

	w.RegisterActivity(&FailureActivities{WebhookURL: cfg.FailureWebhookURL})

	w.RegisterActivity(newResolveControlActivities(cfg.K8sDatasetParent))
	w.RegisterActivity(newPackActivities())
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

	// Write-health measurement (SPEC §14) runs after each tape's write window and
	// exports its gauges against the worker's Prometheus registry.
	w.RegisterActivity(newWriteHealthActivities(cfg.MetricsRegisterer))

	w.RegisterActivity(newEjectActivities())

	// Report and Deliver run on the data worker: the report/ISO build needs the
	// staged files, recovery binaries, and pinned tools that live here, and
	// delivering from here keeps the tens-of-MB ISO off the Temporal payload path
	// (SPEC §4.3 phases 9–10, §9–§11).
	w.RegisterActivity(newReportActivities(cfg.StagingDir, cfg.RecoveryBinariesDir, cfg.RecoverySourcesDir))
	w.RegisterActivity(newDeliverActivities())

	// The per-disc optical burn/verify activities run here too: the burners are
	// attached to the storage host and the recovery ISO is staged beside the run's
	// tree (SPEC §10). They are stateless — every per-disc parameter (device,
	// ISO/manifest path, AllowNonBlankDiscs) flows through the activity input — so
	// nothing is wired in from DataConfig.
	w.RegisterActivity(newBurnActivities())
}
