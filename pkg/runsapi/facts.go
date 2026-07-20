// This file implements phases.go's per-phase fact extraction (issue #273
// AC2): a small, fixed set of observable facts recoverable from each phase's
// own activity Input/Result payloads — e.g. Resolve's archive count, Verify's
// matched-file count, Pack's logical-tape/copy count. Facts are computed
// opportunistically and best-effort: a phase that has not produced the
// underlying activity result yet (still running, or failed before reaching
// it) simply yields fewer facts, never an error.
package runsapi

import (
	"fmt"

	commonpb "go.temporal.io/api/common/v1"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// writePathPausePhase decodes writepause.go's WritePathPauseInput to recover
// which sub-phase ("Load" or "Write") a NotifyWritePathPause alert belongs to.
// Defaults to PhaseWrite — the more common of the two failure sites, and a
// harmless default relative to a genuinely undecodable/older-version input —
// rather than dropping the activity from the timeline entirely.
func writePathPausePhase(input *commonpb.Payloads) string {
	var decoded struct{ Phase string }
	if err := decodePayloads(input, &decoded); err == nil && decoded.Phase == backup.PhaseLoad {
		return backup.PhaseLoad
	}

	return backup.PhaseWrite
}

// findByName returns every record in records whose Name matches, in the order
// given (already schedule-ordered by buildPhaseTimeline).
func findByName(records []activityRecord, name string) []activityRecord {
	var matches []activityRecord

	for _, record := range records {
		if record.Name == name {
			matches = append(matches, record)
		}
	}

	return matches
}

// last returns the last element of records, and false for an empty slice.
func last(records []activityRecord) (activityRecord, bool) {
	if len(records) == 0 {
		return activityRecord{}, false
	}

	return records[len(records)-1], true
}

// phaseFacts dispatches to the per-phase fact extractor for name, given that
// phase's own activity records (already filtered/ordered by
// buildPhaseTimeline) and, for the Write phase only, the whole run's derived
// tape outcomes (deriveTapeOutcomes spans Load+Write-phase activities
// together, so it cannot be computed from records alone).
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

// resolveFacts reports the resolved archive count from ResolveAndCheck's
// completed result (resolve.go) — the final, data-side-verified work list
// (rather than ResolveK8sSources' control-side-only partial list).
func resolveFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "ResolveAndCheck"))
	if !ok || !record.Completed {
		return nil
	}

	var archives []backup.ResolvedArchive
	if err := decodePayloads(record.Result, &archives); err != nil {
		return nil
	}

	return []PhaseFact{intFact("archives", "Archives", len(archives))}
}

// prepareFacts reports the staged archive count and total staged bytes from
// PrepareArchives' completed result (prepare.go).
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

// humanizeBytes renders a byte count in decimal (SI) units — matching the
// decimal capacities the tape world quotes (an LTO-9 tape is 18 TB, not
// TiB) — for compact display. The exact count travels alongside it in the
// fact's Title for hover text, so this can round freely.
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

// groupDigits renders n with thousands separators (e.g. 6000000000 ->
// "6,000,000,000") so the exact byte count in a fact's hover Title stays
// legible.
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

// packFacts reports the logical tape count and copy count from Pack's
// completed result (plan.go).
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

// par2Facts reports the recovery-set count from GeneratePAR2's completed
// result (par2.go) — one set per staged archive.
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

// verifyFacts reports the planned file count (slices + PAR2 files across
// every archive) from Verify's own scheduled Input (verify.go's VerifyInput)
// — available as soon as the activity is scheduled, unlike the activity's
// Result, which carries no count at all (VerifiedPlan is an empty struct;
// verify() only ever returns success or a specific mismatch error). When the
// activity has completed successfully every planned file is, by definition,
// verified to match — matchedFiles equals the planned count and is reported
// as "N/N", matching the design's "71/71 matched" fact
// (DESIGN_ANALYSIS.md §5).
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

// loadFacts reports the total tapes loaded across every drive-set (SPEC §4.3
// phases 6-8 interleave Load per set), summing every completed Load
// activity's result (library.go).
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

// writeFacts reports the written/failed tape counts from the whole run's
// derived tape outcomes (tapes.go) — outcomes correlate Load's per-tape
// barcodes with the Write-phase activities that finish them, so it cannot be
// computed from the Write phase's own activity records alone.
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

// ejectFacts reports the exported-tape count from the most recent Eject
// activity's completed result (library.go's EjectResult.InIOStation is
// cumulative across every Eject call so far in the run).
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

// reportFacts reports whether the report (and, when optical burning is
// configured, the recovery ISO) were built, from BuildReport's completed
// result (report.go).
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

// burnFacts reports the burned-disc count, or that optical burning is
// disabled for this run. Zero Burn-phase activity is ambiguous on its own —
// it means either "not reached yet" or "ran as a no-op" (burnPhase returns
// immediately when delivery.opticalBurn is unset, burnpath.go) — so the
// caller's already-resolved PhaseStatus disambiguates: only a completed phase
// with no activity is reported as disabled.
func burnFacts(status PhaseStatus, records []activityRecord) []PhaseFact {
	if len(records) == 0 {
		if status == PhaseCompleted {
			return []PhaseFact{{Key: "opticalBurn", Label: "Optical burn", Value: "disabled"}}
		}

		return nil
	}

	// Count only completed BurnDisc activities, not every scheduled attempt:
	// a failed-and-retried burn would otherwise inflate the disc count above
	// the number of discs actually written, the same completed-only filter
	// loadFacts and ejectFacts already apply.
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

// deliverFacts reports whether the report was delivered, from Deliver's
// terminal state (deliver.go's Deliver activity returns no meaningful result,
// only success/failure).
func deliverFacts(records []activityRecord) []PhaseFact {
	record, ok := last(findByName(records, "Deliver"))
	if !ok || !record.Completed {
		return nil
	}

	return []PhaseFact{{Key: "delivered", Label: "Delivered", Value: yesNo(true)}}
}

// yesNo renders a bool as the same "yes"/"no" convention every fact above
// uses instead of a raw "true"/"false".
func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}
