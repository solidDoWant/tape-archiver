package envvar

import "os"

// Config holds configuration parsed from environment variables.
type Config struct {
	// DiscordFailureWebhookURL is the webhook URL for run failure alerts.
	// When empty, failure alerting is disabled — no error is raised on absence.
	DiscordFailureWebhookURL string
	// K8sDatasetParent is democratic-csi's datasetParentName, prepended to a
	// relative CSI snapshotHandle to rebuild the absolute ZFS snapshot path
	// during k8s resolution (SPEC.md §3). Empty treats handles as already
	// absolute; it is only needed when a run names k8s sources.
	K8sDatasetParent string
	// StagingDir is the directory on the data worker the Prepare phase stages
	// prepared archives into — a plain subdirectory of an existing dataset (e.g.
	// /mnt/bulk-pool-01/archive/.tape-staging), bind-mounted into the worker
	// container (SPEC.md §4.1). Each run isolates its output in a subdirectory
	// keyed by run id. Required on the data worker; the Prepare activity fails
	// when it is empty.
	StagingDir string
	// RecoveryBinariesDir is the directory on the data worker holding the
	// statically linked recovery binaries (age, par2, zstd, tar) the Report phase
	// stages into the recovery ISO (SPEC.md §10). Required on the data worker; the
	// Report activity fails when it is empty or a binary is not static.
	RecoveryBinariesDir string
	// RecoverySourcesDir is the directory on the data worker holding the recovery
	// tools' upstream source archives the Report phase stages into the recovery
	// ISO's src/ (SPEC.md §2, §10 — "…plus their source"). It is the sibling $out/src
	// of RecoveryBinariesDir's $out/bin from nix/recovery-binaries.nix. Required on
	// the data worker; the Report activity fails when it is empty or yields no files.
	RecoverySourcesDir string
}

// Parse reads operational configuration from environment variables.
func Parse() (Config, error) {
	return Config{
		DiscordFailureWebhookURL: os.Getenv("DISCORD_FAILURE_WEBHOOK_URL"),
		K8sDatasetParent:         os.Getenv("TAPE_K8S_DATASET_PARENT"),
		StagingDir:               os.Getenv("TAPE_STAGING_DIR"),
		RecoveryBinariesDir:      os.Getenv("TAPE_RECOVERY_BINARIES_DIR"),
		RecoverySourcesDir:       os.Getenv("TAPE_RECOVERY_SOURCES_DIR"),
	}, nil
}
