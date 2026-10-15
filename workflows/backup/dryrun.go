package backup

import (
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
)

// MemoKeyDryRun is the memo field recording whether a run is a dry run; it is the
// only durable signal distinguishing one from a production run.
const MemoKeyDryRun = "dryRun"

// isDryRun reports whether this run was submitted as a dry run. The memo is part
// of the start event, so this is deterministic.
func isDryRun(ctx workflow.Context) bool {
	payload, ok := workflow.GetInfo(ctx).Memo.GetFields()[MemoKeyDryRun]
	if !ok {
		return false
	}

	var dryRun bool
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &dryRun); err != nil {
		return false
	}

	return dryRun
}
