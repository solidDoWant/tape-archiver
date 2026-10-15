// Per-phase fact extraction (issue #273) from activity Input/Result payloads.
// Best-effort: a phase without the underlying result yields fewer facts, never
// an error.
package runsapi

import (
	"fmt"

	commonpb "go.temporal.io/api/common/v1"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// writePathPausePhase returns which sub-phase a NotifyWritePathPause alert
// belongs to, defaulting to Write for an undecodable input.
func writePathPausePhase(input *commonpb.Payloads) string {
	var decoded struct{ Phase string }
	if err := decodePayloads(input, &decoded); err == nil && decoded.Phase == backup.PhaseLoad {
		return backup.PhaseLoad
	}

	return backup.PhaseWrite
}

func findByName(records []activityRecord, name string) []activityRecord {
	var matches []activityRecord

	for _, record := range records {
		if record.Name == name {
			matches = append(matches, record)
		}
	}

	return matches
}

func last(records []activityRecord) (activityRecord, bool) {
	if len(records) == 0 {
		return activityRecord{}, false
	}

	return records[len(records)-1], true
}

// phaseFacts dispatches to the per-phase extractor. Write uses the run-wide
// tape outcomes, which span Load and Write activities.
func phaseFacts(name string, status PhaseStatus, records []activityRecord, outcomes []TapeOutcome) []PhaseFact {
	switch name {
	case backup.PhaseResolve:
		return resolveFacts(records)
	case backup.PhasePrepare:
		return prepareFacts(records)
	case backup.PhasePack:
		return packFacts(records)
	case backup.PhaseGeneratePAR2:
		return par2Facts(records)
	case backup.PhaseVerify:
		return verifyFacts(records)
	case backup.PhaseLoad:
		return loadFacts(records)
	case backup.PhaseWrite:
		return writeFacts(outcomes)
	case backup.PhaseEject:
		return ejectFacts(records)
	case backup.PhaseReport:
		return reportFacts(records)
	case backup.PhaseBurn:
		return burnFacts(status, records)
	case backup.PhaseDeliver:
		return deliverFacts(records)
	default:
		return nil
	}
}

// bytesPerGiB: derived slice sizes are whole GiB (backup.DeriveSliceSize).
const bytesPerGiB = 1 << 30

// resolveFacts uses ResolveAndCheck's data-side-verified result. The slice
// size (issue #324) is recomputed with the same pure DeriveSliceSize the
// workflow uses, so no workflow query is needed.
func resolveFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "ResolveAndCheck"))
	if !ok || !record.Completed {
		return nil
	}

	var archives []backup.ResolvedArchive
	if err := decodePayloads(record.Result, &archives); err != nil {
		return nil
	}

	sliceSize := backup.DeriveSliceSize(archives)

	return []PhaseFact{
		intFact("archives", "Archives", len(archives)),
		{
			Key:   "sliceSize",
			Label: "Slice size",
			Value: fmt.Sprintf("%d GiB", sliceSize/bytesPerGiB),
			Title: fmt.Sprintf("%s bytes", groupDigits(sliceSize)),
		},
	}
}

func prepareFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "PrepareArchives"))
	if !ok || !record.Completed {
		return nil
	}

	var staged []backup.StagedArchive
	if err := decodePayloads(record.Result, &staged); err != nil {
		return nil
	}

	var totalBytes int64
	for _, archive := range staged {
		totalBytes += archive.SizeBytes
	}

	return []PhaseFact{
		intFact("archivesStaged", "Archives staged", len(staged)),
		{
			Key:   "stagedBytes",
			Label: "Staged bytes",
			Value: humanizeBytes(totalBytes),
			Title: fmt.Sprintf("%s bytes", groupDigits(totalBytes)),
		},
	}
}

// humanizeBytes uses decimal (SI) units, matching how tape capacities are quoted.
func humanizeBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// groupDigits renders n with thousands separators, e.g. "6,000,000,000".
func groupDigits(n int64) string {
	digits := fmt.Sprintf("%d", n)

	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}

	var grouped []byte

	for i, d := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}

		grouped = append(grouped, d)
	}

	return sign + string(grouped)
}

func packFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "Pack"))
	if !ok || !record.Completed {
		return nil
	}

	var plan backup.TapePlan
	if err := decodePayloads(record.Result, &plan); err != nil {
		return nil
	}

	return []PhaseFact{
		intFact("logicalTapes", "Logical tapes", len(plan.Tapes)),
		intFact("copies", "Copies", plan.Copies),
	}
}

func par2Facts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "GeneratePAR2"))
	if !ok || !record.Completed {
		return nil
	}

	var sets []backup.PAR2Set
	if err := decodePayloads(record.Result, &sets); err != nil {
		return nil
	}

	return []PhaseFact{intFact("recoverySets", "Recovery sets", len(sets))}
}

// verifyFacts counts planned files from Verify's Input, since its Result is
// empty. A successful Verify means every planned file matched, reported "N/N".
func verifyFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "Verify"))
	if !ok {
		return nil
	}

	var input backup.VerifyInput
	if err := decodePayloads(record.Input, &input); err != nil {
		return nil
	}

	var total int
	for _, archive := range input.Archives {
		total += len(archive.Slices)
	}

	for _, set := range input.PAR2 {
		total += len(set.Files)
	}

	if !record.Completed {
		return []PhaseFact{intFact("filesPlanned", "Files planned", total)}
	}

	return []PhaseFact{{Key: "filesVerified", Label: "Files verified", Value: fmt.Sprintf("%d/%d", total, total)}}
}

// loadFacts sums every completed Load across the interleaved drive-sets.
func loadFacts(records []activityRecord) []PhaseFact {
	var total int

	for _, record := range findByName(records, "Load") {
		if !record.Completed {
			continue
		}

		var loaded []backup.LoadedTape
		if err := decodePayloads(record.Result, &loaded); err != nil {
			continue
		}

		total += len(loaded)
	}

	if total == 0 {
		return nil
	}

	return []PhaseFact{intFact("tapesLoaded", "Tapes loaded", total)}
}

func writeFacts(outcomes []TapeOutcome) []PhaseFact {
	if len(outcomes) == 0 {
		return nil
	}

	var written, failed int

	for _, outcome := range outcomes {
		switch outcome.Result {
		case tapeOutcomeWritten:
			written++
		case tapeOutcomeFailed:
			failed++
		}
	}

	facts := []PhaseFact{intFact("tapesWritten", "Tapes written", written)}
	if failed > 0 {
		facts = append(facts, intFact("tapesFailed", "Tapes failed", failed))
	}

	return facts
}

// ejectFacts uses the most recent Eject, since EjectResult.InIOStation is cumulative.
func ejectFacts(records []activityRecord) []PhaseFact {
	var mostRecent *backup.EjectResult

	for _, record := range findByName(records, "Eject") {
		if !record.Completed {
			continue
		}

		var result backup.EjectResult
		if err := decodePayloads(record.Result, &result); err != nil {
			continue
		}

		mostRecent = &result
	}

	if mostRecent == nil {
		return nil
	}

	return []PhaseFact{intFact("tapesExported", "Tapes exported", len(mostRecent.InIOStation))}
}

func reportFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "BuildReport"))
	if !ok || !record.Completed {
		return nil
	}

	var output backup.ReportOutput
	if err := decodePayloads(record.Result, &output); err != nil {
		return nil
	}

	facts := []PhaseFact{{Key: "reportBuilt", Label: "Report built", Value: yesNo(output.ReportPath != "")}}
	if output.UncompressedISOPath != "" {
		facts = append(facts, PhaseFact{Key: "isoBuilt", Label: "Recovery ISO built", Value: yesNo(true)})
	}

	return facts
}

// burnFacts: zero Burn activity means either "not reached" or "disabled"
// (a no-op phase); only a completed phase is reported as disabled.
func burnFacts(status PhaseStatus, records []activityRecord) []PhaseFact {
	if len(records) == 0 {
		if status == PhaseCompleted {
			return []PhaseFact{{Key: "opticalBurn", Label: "Optical burn", Value: "disabled"}}
		}

		return nil
	}

	// Completed only, so retried burns do not inflate the count.
	discs := 0

	for _, record := range findByName(records, "BurnDisc") {
		if record.Completed {
			discs++
		}
	}

	if discs == 0 {
		return nil
	}

	return []PhaseFact{intFact("discsBurned", "Discs burned", discs)}
}

func deliverFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "Deliver"))
	if !ok || !record.Completed {
		return nil
	}

	return []PhaseFact{{Key: "delivered", Label: "Delivered", Value: yesNo(true)}}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}
