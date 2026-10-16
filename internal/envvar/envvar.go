package envvar

import "os"

// Config holds configuration parsed from environment variables.
type Config struct {
	// DiscordFailureWebhookURL receives run failure alerts; empty disables them.
	DiscordFailureWebhookURL string
	// K8sDatasetParent is democratic-csi's datasetParentName, prepended to a
	// relative CSI snapshotHandle. Empty treats handles as absolute.
	K8sDatasetParent string
	// K8sCSIDriver is the CSI driver name PersistentVolumeClaim sources must use;
	// empty means k8ssnap.DefaultCSIDriver.
	K8sCSIDriver string
	// StagingDir is where Prepare stages archives, one subdirectory per run.
	// Required on the data worker.
	StagingDir string
	// RecoveryBinariesDir holds the statically linked recovery tools staged into
	// the recovery ISO. Required on the data worker.
	RecoveryBinariesDir string
	// RecoverySourcesDir holds the recovery tools' source archives staged into
	// the ISO's src/. Required on the data worker.
	RecoverySourcesDir string
}

// Parse reads operational configuration from environment variables.
func Parse() (Config, error) {
	return Config{
		DiscordFailureWebhookURL: os.Getenv("DISCORD_FAILURE_WEBHOOK_URL"),
		K8sDatasetParent:         os.Getenv("TAPE_K8S_DATASET_PARENT"),
		K8sCSIDriver:             os.Getenv("TAPE_K8S_CSI_DRIVER"),
		StagingDir:               os.Getenv("TAPE_STAGING_DIR"),
		RecoveryBinariesDir:      os.Getenv("TAPE_RECOVERY_BINARIES_DIR"),
		RecoverySourcesDir:       os.Getenv("TAPE_RECOVERY_SOURCES_DIR"),
	}, nil
}
