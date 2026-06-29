package main

import (
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/envvar"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// registerActivities registers the backup workflow and its activities for the
// given role onto w. The per-role registration lives in the backup package
// (workflows/backup), which owns the workflow and its phase activities; this is
// the seam through which the worker binary hands its operational configuration
// to that package.
//
// The phase activities are stubs until each phase sub-issue lands; the failure
// alert (SPEC §11) and the workflow backbone are real.
func registerActivities(w worker.Worker, role Role, env envvar.Config) {
	switch role {
	case RoleControl:
		registerControlActivities(w, env)
	case RoleData:
		registerDataActivities(w, env)
	}
}

// registerControlActivities registers the control-role surface: the Backup
// workflow plus the control-queue activities (VolumeSnapshot resolution,
// PDF report building, recovery ISO building, Discord delivery) and the
// operational failure-alert activity, wired with the failure webhook URL.
func registerControlActivities(w worker.Worker, env envvar.Config) {
	backup.RegisterControl(w, backup.ControlConfig{
		FailureWebhookURL: env.DiscordFailureWebhookURL,
		K8sDatasetParent:  env.K8sDatasetParent,
	})
}

// registerDataActivities registers the data-queue activities: tar/compress/split
// (pkg/archive), age encryption (pkg/agewrap), PAR2 generation (pkg/par2),
// checksum verification (pkg/checksum), LTFS format/mount/write/unmount
// (pkg/ltfs), and changer load/unload (pkg/tape), wired with the data worker's
// staging directory.
func registerDataActivities(w worker.Worker, env envvar.Config) {
	backup.RegisterData(w, backup.DataConfig{
		StagingDir: env.StagingDir,
	})
}
