package main

import "go.temporal.io/sdk/worker"

// registerActivities registers the activities for the given role onto w.
//
// The activity implementations themselves are owned by the backup workflow
// (issue #17), which wires each phase's activities into the per-role seams
// below. Until #17 lands these seams are intentionally empty: a Temporal worker
// starts and polls its task queue with no activities registered, which is the
// handoff point #17 is written to expect ("Activities are wired to the control
// and data workers from issue #15").
func registerActivities(w worker.Worker, role Role) {
	switch role {
	case RoleControl:
		registerControlActivities(w)
	case RoleData:
		registerDataActivities(w)
	}
}

// registerControlActivities registers the control-queue activities:
// VolumeSnapshot resolution (pkg/k8ssnap), PDF report building (pkg/report),
// recovery ISO building (pkg/recoverykit), and Discord delivery (pkg/webhook).
//
// TODO(#17): register the activities as the backup workflow defines them.
func registerControlActivities(_ worker.Worker) {}

// registerDataActivities registers the data-queue activities: tar/compress/split
// (pkg/archive), age encryption (pkg/agewrap), PAR2 generation (pkg/par2),
// checksum verification (pkg/checksum), LTFS format/mount/write/unmount
// (pkg/ltfs), and changer load/unload (pkg/tape).
//
// TODO(#17): register the activities as the backup workflow defines them.
func registerDataActivities(_ worker.Worker) {}
