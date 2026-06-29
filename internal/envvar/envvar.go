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
}

// Parse reads operational configuration from environment variables.
func Parse() (Config, error) {
	return Config{
		DiscordFailureWebhookURL: os.Getenv("DISCORD_FAILURE_WEBHOOK_URL"),
		K8sDatasetParent:         os.Getenv("TAPE_K8S_DATASET_PARENT"),
	}, nil
}
