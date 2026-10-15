package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/envvar"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// registerActivities registers the role's workflow and activities onto w.
// metricsReg is nil when metrics are disabled.
func registerActivities(w worker.Worker, role Role, env envvar.Config, metricsReg prometheus.Registerer) {
	switch role {
	case RoleControl:
		registerControlActivities(w, env)
	case RoleData:
		registerDataActivities(w, env, metricsReg)
	}
}

func registerControlActivities(w worker.Worker, env envvar.Config) {
	backup.RegisterControl(w, backup.ControlConfig{
		FailureWebhookURL: env.DiscordFailureWebhookURL,
		K8sDatasetParent:  env.K8sDatasetParent,
	})
}

func registerDataActivities(w worker.Worker, env envvar.Config, metricsReg prometheus.Registerer) {
	backup.RegisterData(w, backup.DataConfig{
		StagingDir:          env.StagingDir,
		RecoveryBinariesDir: env.RecoveryBinariesDir,
		RecoverySourcesDir:  env.RecoverySourcesDir,
		MetricsRegisterer:   metricsReg,
	})
}
