package backup

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/buildinfo"
	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
	"github.com/solidDoWant/tape-archiver/pkg/recoverykit"
	"github.com/solidDoWant/tape-archiver/pkg/report"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// The Report phase (SPEC §4.3 phase 9) builds the run's durable recovery artifacts:
// always the PDF report (§9), and — only when optical burning is enabled
// (delivery.opticalBurn) — the recovery ISO (§10) as the mountable image the Burn
// phase consumes. The burned disc is the ISO's durable home, so a run without
// burning produces no ISO at all. It runs on the data worker (SPEC §4.1) — not the
// control worker — because everything it consumes lives there: the staged
// slice/PAR2 files, the pinned recovery binaries staged into the ISO, and the
// external tools whose versions the report records. The LTFS indexes travel in run
// state from the Write phase. Building here also keeps the tens-of-MB ISO off the
// Temporal payload path and out of the control image (which would otherwise have to
// duplicate the recovery binaries).
//
// Before building anything it enforces the key-escrow contract (SPEC §7): the run
// config's private identity must be present and must be the identity for one of
// the configured recipients, so the report and ISO escrow a key that can actually
// decrypt the archives. A mismatch fails the phase.
//
// The Report phase does no tape or device I/O beyond a best-effort SCSI INQUIRY
// for drive/library provenance; it is pure assembly over already-staged, already-
// verified inputs.

const (
	// reportTimeout bounds the Report activity: rendering the PDF and assembling the
	// ISO 9660 image, over tens of MB. An hour is far more than enough while still
	// bounding a hang.
	reportTimeout = 1 * time.Hour

	// reportFileName and isoFileName are the staged artifact names under the run's
	// staging directory. The PDF report is always produced and delivered (SPEC §11);
	// the uncompressed ISO is staged only when optical burning is enabled, as the
	// mountable image the Burn phase consumes.
	reportFileName = "report.pdf"
	isoFileName    = "recovery.iso"
	// discManifestFileName is the staged sha256sum manifest of the recovery ISO's
	// contents, written beside the uncompressed ISO only when optical burning is
	// enabled. The Burn phase verifies each burned disc against it (SPEC §10).
	discManifestFileName = "disc-manifest.sha256"

	// unknownIdentity is the placeholder for a drive/library identifier that could
	// not be read, so the report never renders a blank provenance field.
	unknownIdentity = "unknown"
)

// recoveryProcedure is the concise, step-by-step recovery text rendered into the
// PDF report (SPEC §9) so the laminated printout is self-contained even if the
// disc is lost. The full procedure — including index-loss recovery and the
// failure-scenario handling — ships on the recovery disc as recovery-procedure.md
// (embedded by pkg/recoverykit from docs/recovery-procedure.md, SPEC §10); this
// concise version points there for those cases. It refers only to the tools
// staged on the disc under bin/ and the artifacts beside it.
const recoveryProcedure = `Tape Archiver — recovery procedure (concise)

You need only this disc and the physical tapes. All tools referenced below ship
statically linked under bin/ on this disc; the age private identity is printed in
report.pdf (and in the "Encryption key" section of this text's PDF). The full
procedure — index-loss recovery and failure handling — is in recovery-procedure.md
on this disc.

1.  Load a tape into a standalone LTO drive of the generation named in the report's
    build parameters (a newer generation that can read it also works).
2.  Mount the tape's LTFS volume read-only:  ltfs -o ro <mountpoint>
    If the on-tape LTFS index is damaged, see recovery-procedure.md (ltfsck
    --deep-recovery, or the captured ltfs-index/<barcode>.schema extents).
3.  Copy the tape's files to disk into a directory named for the tape's barcode
    (printed on the tape and in the report), preserving the archives/ tree, so
    the layout matches manifest.sha256 (see step 4). Each archive then lives
    under <barcode>/archives/NNN-<label>/ (NNN is the source index; <label> is a
    descriptive name); the per-tape manifest.json lists every file and its
    SHA-256. For example, for tape TAPE01L6:
        mkdir -p TAPE01L6 && cp -r <mountpoint>/archives TAPE01L6/
4.  Verify the copied files against manifest.sha256 with the system sha256sum
    (coreutils, on any Linux host). Every line is <barcode>/archives/NNN-<label>/<file>,
    so run it from the parent directory holding the barcode-named tape directories:
        sha256sum -c /path/to/disc/manifest.sha256
    One manifest covers every tape, so if you have copied only some tapes the
    not-yet-copied lines report as missing (a non-zero exit). To verify a single
    tape, filter its lines by barcode (works on any coreutils):
        grep '  TAPE01L6/' /path/to/disc/manifest.sha256 | sha256sum -c -
    (On modern coreutils, sha256sum -c --ignore-missing /path/to/disc/manifest.sha256
    verifies everything copied so far in one pass.)
    If any archive slice or its PAR2 files are corrupt, repair with:
        bin/par2 repair <barcode>/archives/NNN-<label>/archive.par2
5.  Concatenate an archive's slices in order to reconstruct its age stream:
        cat <barcode>/archives/NNN-<label>/archive.000 <barcode>/archives/NNN-<label>/archive.001 ... > archive.age
6.  Decrypt with the escrowed identity (save it to identity.txt first):
        bin/age -d -i identity.txt -o archive.tar.zst archive.age
7.  If the archive was compressed, decompress it:
        bin/zstd -d archive.tar.zst -o archive.tar
    (An archive stored uncompressed is already a tar after step 6.)
8.  Unpack the tar:
        bin/tar -xf archive.tar
    A snapshot group unpacks to one subdirectory per member volume.
9.  Repeat for every archive on every tape listed in the report.`

// ReportActivities hosts the data-side Report activity. stagingRoot is where the
// run's artifacts are written (beside its staged tree); binariesDir holds the
// static recovery binaries and sourcesDir the tools' source archives, both staged
// into the ISO (SPEC §10).
type ReportActivities struct {
	stagingRoot string
	binariesDir string
	sourcesDir  string
}

// newReportActivities returns the production data-side Report activity.
func newReportActivities(stagingRoot, binariesDir, sourcesDir string) *ReportActivities {
	return &ReportActivities{stagingRoot: stagingRoot, binariesDir: binariesDir, sourcesDir: sourcesDir}
}

// ReportInput is the payload for the Report activity: the run config and the full
// run state the report and ISO are assembled from. RunID and Date come from the
// workflow (deterministic) so the artifact is stamped identically on any retry.
type ReportInput struct {
	Config   config.Config
	RunID    string
	Date     time.Time
	Resolved []ResolvedArchive
	Staged   []StagedArchive
	PAR2     []PAR2Set
	Plan     TapePlan
	Written  []WrittenTape
	// Discs are the recovery discs burned for the run, populated only for the
	// post-burn re-render of the delivered report (the Report phase itself runs
	// before the Burn phase, so it leaves this empty). When set, the report records
	// a Discs section noting each burner and any deliberate overwrite (SPEC §10).
	Discs []BurnResult
}

// ReportOutput is the Report activity's result: the on-disk path of the PDF report
// the Deliver phase uploads, plus the optional uncompressed ISO and disc manifest
// staged for the Burn phase when optical burning is enabled.
type ReportOutput struct {
	// ReportPath is the staged PDF report (SPEC §9), always produced and delivered.
	ReportPath string
	// UncompressedISOPath is the staged uncompressed recovery ISO 9660 image, set
	// only when optical burning is enabled (delivery.opticalBurn) — the mountable
	// image the Burn phase burns. Empty when burning is disabled.
	UncompressedISOPath string
	// DiscManifestPath is the staged sha256sum manifest of the recovery ISO's
	// contents, set only when optical burning is enabled. The Burn phase passes it
	// to VerifyDisc to read back and verify each burned disc. Empty when burning is
	// disabled.
	DiscManifestPath string
}

// BuildReport builds the PDF report for the run (SPEC §4.3 phase 9), and — only
// when optical burning is enabled — the recovery ISO (SPEC §10) as the mountable
// image the Burn phase consumes. It stages the artifacts beside the run's prepared
// tree and returns their paths. It fails if the escrow identity is missing or does
// not match a configured recipient.
func (a *ReportActivities) BuildReport(ctx context.Context, input ReportInput) (ReportOutput, error) {
	if a.stagingRoot == "" {
		return ReportOutput{}, fmt.Errorf("staging directory is not configured (set TAPE_STAGING_DIR on the data worker)")
	}

	if a.binariesDir == "" {
		return ReportOutput{}, fmt.Errorf("recovery binaries directory is not configured (set TAPE_RECOVERY_BINARIES_DIR on the data worker)")
	}

	if a.sourcesDir == "" {
		return ReportOutput{}, fmt.Errorf("recovery sources directory is not configured (set TAPE_RECOVERY_SOURCES_DIR on the data worker)")
	}

	outDir := filepath.Join(a.stagingRoot, activity.GetInfo(ctx).WorkflowExecution.RunID)

	// Emit a liveness heartbeat while rendering the report and assembling the ISO
	// so a data-worker restart mid-phase is caught within activityHeartbeatTimeout
	// rather than the 1h StartToClose.
	var output ReportOutput

	err := withActivityHeartbeat(ctx, func() error {
		var err error

		output, err = a.buildReport(ctx, outDir, input)

		return err
	})

	return output, err
}

// buildReport is the body of the Report activity, split out so it can be exercised
// against a real directory without an activity context.
func (a *ReportActivities) buildReport(ctx context.Context, outDir string, input ReportInput) (ReportOutput, error) {
	if err := verifyEscrowIdentity(ctx, input.Config.Encryption); err != nil {
		return ReportOutput{}, err
	}

	if len(input.Written) == 0 {
		return ReportOutput{}, fmt.Errorf("no tapes were written; there is nothing to report")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return ReportOutput{}, fmt.Errorf("create report output directory %q: %w", outDir, err)
	}

	// Render the PDF to memory so the same bytes are both written to disk and (when
	// burning) embedded in the ISO (SPEC §10) — no round-trip through the filesystem.
	pdf, err := renderReportPDF(ctx, input)
	if err != nil {
		return ReportOutput{}, err
	}

	reportPath := filepath.Join(outDir, reportFileName)
	if err := os.WriteFile(reportPath, pdf, 0o644); err != nil {
		return ReportOutput{}, fmt.Errorf("write PDF report to %q: %w", reportPath, err)
	}

	// The recovery ISO is built only when optical burning is enabled: the burned
	// disc is the ISO's durable home (SPEC §10, §11), so a run without burning
	// produces just the report.
	if !input.Config.Delivery.OpticalBurn.Enabled() {
		slog.Info("report: built recovery artifacts", "report", reportPath)

		return ReportOutput{ReportPath: reportPath}, nil
	}

	sha256Manifest, err := buildSHA256Manifest(input)
	if err != nil {
		return ReportOutput{}, fmt.Errorf("build SHA-256 manifest: %w", err)
	}

	uncompressedISOPath := filepath.Join(outDir, isoFileName)

	indexes, err := tapeIndexes(input.Written)
	if err != nil {
		return ReportOutput{}, fmt.Errorf("collect tape indexes: %w", err)
	}

	isoInput := recoverykit.Input{
		Report:      pdf,
		Manifest:    sha256Manifest,
		TapeIndexes: indexes,
		BinariesDir: a.binariesDir,
		SourcesDir:  a.sourcesDir,
	}

	discManifest, err := buildRecoveryISO(ctx, isoInput, uncompressedISOPath)
	if err != nil {
		return ReportOutput{}, err
	}

	// Stage the disc-content manifest beside the uncompressed ISO so the Burn phase
	// can verify each burned disc against it (SPEC §10). It lists the ISO's own files
	// (report.pdf, manifest.sha256, …), distinct from the on-tape SHA-256 manifest
	// embedded inside the ISO.
	discManifestPath := filepath.Join(outDir, discManifestFileName)
	if err := os.WriteFile(discManifestPath, discManifest.Bytes(), 0o644); err != nil {
		return ReportOutput{}, fmt.Errorf("write disc-content manifest to %q: %w", discManifestPath, err)
	}

	slog.Info("report: built recovery artifacts", "report", reportPath, "iso", uncompressedISOPath)

	return ReportOutput{
		ReportPath:          reportPath,
		UncompressedISOPath: uncompressedISOPath,
		DiscManifestPath:    discManifestPath,
	}, nil
}

// renderReportPDF renders the run's PDF report to bytes (SPEC §9). It is shared
// by the Report phase (which also embeds the bytes in the recovery ISO) and the
// post-burn re-render of the delivered report (which records the burned discs),
// so both produce byte-identical PDFs from the same run state. The escrow-identity
// contract is enforced by the caller before staging anything.
func renderReportPDF(ctx context.Context, input ReportInput) ([]byte, error) {
	manifest := buildReportManifest(input, queryDeviceIdentity(ctx, input.Config.Library))

	var pdf bytes.Buffer
	if err := report.Build(manifest, &pdf); err != nil {
		return nil, fmt.Errorf("build PDF report: %w", err)
	}

	return pdf.Bytes(), nil
}

// RebuildDeliveredReport re-renders the delivered PDF report from the full run
// state now that the Burn phase has run, overwriting the staged report.pdf so the
// delivered report records the burned discs and any deliberate disc overwrite
// (SPEC §10). Only the delivered PDF is re-rendered — the recovery ISO (and the
// pre-burn PDF copy inside it, which necessarily predates the burn) is left as-is.
// It returns the path of the re-rendered report. The phase orders Report → Burn →
// this re-render → Deliver.
func (a *ReportActivities) RebuildDeliveredReport(ctx context.Context, input ReportInput) (string, error) {
	if a.stagingRoot == "" {
		return "", fmt.Errorf("staging directory is not configured (set TAPE_STAGING_DIR on the data worker)")
	}

	outDir := filepath.Join(a.stagingRoot, activity.GetInfo(ctx).WorkflowExecution.RunID)

	var reportPath string

	err := withActivityHeartbeat(ctx, func() error {
		var err error

		reportPath, err = a.rebuildReport(ctx, outDir, input)

		return err
	})

	return reportPath, err
}

// rebuildReport is the body of the RebuildDeliveredReport activity, split out so
// it can be exercised against a real directory without an activity context. It
// re-renders the delivered PDF into outDir, overwriting the pre-burn report.pdf.
func (a *ReportActivities) rebuildReport(ctx context.Context, outDir string, input ReportInput) (string, error) {
	if err := verifyEscrowIdentity(ctx, input.Config.Encryption); err != nil {
		return "", err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create report output directory %q: %w", outDir, err)
	}

	pdf, err := renderReportPDF(ctx, input)
	if err != nil {
		return "", err
	}

	reportPath := filepath.Join(outDir, reportFileName)
	if err := os.WriteFile(reportPath, pdf, 0o644); err != nil {
		return "", fmt.Errorf("write delivered PDF report to %q: %w", reportPath, err)
	}

	return reportPath, nil
}

// verifyEscrowIdentity enforces the key-escrow contract (SPEC §7): the private
// identity must be present and must derive to one of the configured recipients, so
// the escrowed key can decrypt what the recipients encrypted. It shells out to
// age-keygen (present on the data worker) to derive the recipient.
func verifyEscrowIdentity(ctx context.Context, encryption config.Encryption) error {
	if strings.TrimSpace(encryption.Identity) == "" {
		// An empty escrow identity is a deterministic misconfiguration: every retry
		// re-reads the same empty config and fails identically. Mark it non-retryable
		// so the Report phase fails fast and the SPEC §11 failure alert fires, instead
		// of looping under the server-default unlimited retry policy until the run's
		// timeout (matches prepare.go / session.go / burn.go convention).
		return temporal.NewNonRetryableApplicationError(
			"encryption.identity is empty; the age private identity must be provided to escrow into the report and ISO (SPEC §7)",
			"escrow-identity-invalid",
			nil,
		)
	}

	derived, err := agewrap.RecipientFromIdentity(ctx, encryption.Identity)
	if err != nil {
		// Left retryable: RecipientFromIdentity shells out to age-keygen, so a
		// transient exec failure should still retry.
		return fmt.Errorf("derive recipient from the escrowed identity: %w", err)
	}

	if !slices.Contains(encryption.Recipients, derived) {
		// The escrowed identity derives to a key that is not among the configured
		// recipients — a deterministic config error (a rotated or wrong identity):
		// every retry re-derives the same key and fails identically. Non-retryable so
		// the run fails promptly with the mismatch and the failure alert fires.
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("the escrowed identity's public key (%s) is not among the configured encryption.recipients; it could not decrypt the archives", derived),
			"escrow-identity-invalid",
			nil,
		)
	}

	return nil
}

// buildRecoveryISO assembles the recovery ISO 9660 image once and stages it at
// isoPath for the Burn phase (SPEC §10). It returns the disc-content manifest
// recoverykit.Build produces as it stages each file, which the Burn phase verifies
// each burned disc against.
func buildRecoveryISO(ctx context.Context, isoInput recoverykit.Input, isoPath string) (recoverykit.Manifest, error) {
	out, err := os.Create(isoPath)
	if err != nil {
		return nil, fmt.Errorf("create recovery ISO %q: %w", isoPath, err)
	}

	defer func() { _ = out.Close() }()

	discManifest, err := recoverykit.Build(ctx, isoInput, out)
	if err != nil {
		return nil, fmt.Errorf("build recovery ISO: %w", err)
	}

	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("close recovery ISO %q: %w", isoPath, err)
	}

	return discManifest, nil
}

// tapeIndexes maps each written physical tape to its LTFS index backup for the
// recovery ISO (SPEC §10), keyed by barcode — the canonical physical ID (SPEC §6).
// Each tape's index is read from the path FinalizeTape staged it to on this data
// worker's staging volume, not carried in the activity payload, so the multi-MB
// index never inflates the ReportInput blob (issue #221).
func tapeIndexes(written []WrittenTape) ([]recoverykit.TapeIndex, error) {
	indexes := make([]recoverykit.TapeIndex, 0, len(written))

	for _, writtenTape := range written {
		index, err := os.ReadFile(writtenTape.IndexXMLPath)
		if err != nil {
			return nil, fmt.Errorf("read staged LTFS index for tape %s: %w", writtenTape.Barcode, err)
		}

		indexes = append(indexes, recoverykit.TapeIndex{
			Barcode: string(writtenTape.Barcode),
			Index:   index,
		})
	}

	return indexes, nil
}

// buildReportManifest assembles the report.Manifest from the run state (SPEC §9).
// It is pure — device identifiers are passed in — so it is unit-testable without
// any tooling or hardware.
func buildReportManifest(input ReportInput, device deviceIdentity) report.Manifest {
	resolvedByIndex := make(map[int]ResolvedArchive, len(input.Resolved))
	for _, resolved := range input.Resolved {
		resolvedByIndex[resolved.SourceIndex] = resolved
	}

	par2ByIndex := make(map[int]PAR2Set, len(input.PAR2))
	for _, set := range input.PAR2 {
		par2ByIndex[set.SourceIndex] = set
	}

	nameByIndex := make(map[int]string, len(input.Staged))

	archives := make([]report.Archive, 0, len(input.Staged))

	for _, staged := range input.Staged {
		name := archiveName(input.Config, staged.SourceIndex)
		nameByIndex[staged.SourceIndex] = name

		resolved := resolvedByIndex[staged.SourceIndex]

		archives = append(archives, report.Archive{
			Name:            name,
			Directory:       archiveDirName(staged.SourceIndex, resolved.Label),
			MemberVolumes:   memberVolumes(resolved),
			SourceSnapshots: sourceSnapshots(resolved),
			Files:           archiveFiles(staged, par2ByIndex[staged.SourceIndex]),
		})
	}

	return report.Manifest{
		RunID:             input.RunID,
		Date:              input.Date,
		Archives:          archives,
		Tapes:             reportTapes(input, nameByIndex),
		Discs:             reportDiscs(input.Discs),
		Build:             buildParams(input.Config, device),
		AgeIdentity:       input.Config.Encryption.Identity,
		RecoveryProcedure: recoveryProcedure,
	}
}

// reportDiscs maps the recovery discs burned for the run to the report shape,
// carrying each burner device and whether a non-blank disc was reclaimed (SPEC
// §10). It returns nil for a run without burning so the report omits the Discs
// section entirely (the on-disc, pre-burn report never has one).
func reportDiscs(discs []BurnResult) []report.Disc {
	if len(discs) == 0 {
		return nil
	}

	out := make([]report.Disc, 0, len(discs))
	for _, disc := range discs {
		out = append(out, report.Disc{
			Device:            disc.Device,
			OverwroteNonBlank: disc.OverwroteNonBlank,
		})
	}

	return out
}

// archiveName is the display name of the archive for source index, taken from the
// originating config Source: the ZFS path, the namespaced k8s resource name, or
// the namespaced label selector. It falls back to a positional name if the index
// is somehow out of range.
func archiveName(cfg config.Config, sourceIndex int) string {
	if sourceIndex < 0 || sourceIndex >= len(cfg.Sources) {
		return fmt.Sprintf("sources[%d]", sourceIndex)
	}

	source := cfg.Sources[sourceIndex]

	switch {
	case source.ZFSPath != nil:
		return source.ZFSPath.Name
	case source.K8s != nil && source.K8s.Name != "":
		return fmt.Sprintf("%s/%s", source.K8s.Namespace, source.K8s.Name)
	case source.K8s != nil:
		return fmt.Sprintf("%s [%s]", source.K8s.Namespace, source.K8s.LabelSelector)
	default:
		return fmt.Sprintf("sources[%d]", sourceIndex)
	}
}

// memberVolumes lists the volume identity of each snapshot in the archive, in
// resolution order — the per-member subdirectory name for a group, the volume for
// a single source.
func memberVolumes(resolved ResolvedArchive) []string {
	volumes := make([]string, 0, len(resolved.Snapshots))
	for _, snapshot := range resolved.Snapshots {
		volumes = append(volumes, memberName(snapshot))
	}

	return volumes
}

// sourceSnapshots lists the ZFS snapshot path of each snapshot in the archive.
func sourceSnapshots(resolved ResolvedArchive) []string {
	snapshots := make([]string, 0, len(resolved.Snapshots))
	for _, snapshot := range resolved.Snapshots {
		snapshots = append(snapshots, snapshot.ZFSPath)
	}

	return snapshots
}

// archiveFiles lists the on-tape files for an archive — its slices then its PAR2
// recovery files — each with its size and precomputed SHA-256, by base name (the
// name the file has on tape).
func archiveFiles(staged StagedArchive, par2 PAR2Set) []report.ArchiveFile {
	files := make([]report.ArchiveFile, 0, len(staged.Slices)+len(par2.Files))

	for _, file := range append(append([]StagedSlice{}, staged.Slices...), par2.Files...) {
		files = append(files, report.ArchiveFile{
			Name:   filepath.Base(file.Path),
			Size:   file.SizeBytes,
			SHA256: file.SHA256,
		})
	}

	return files
}

// reportTapes maps each written physical tape (by barcode) to the archive names
// it holds (SPEC §9), derived from the logical tape's placement in the Pack plan.
func reportTapes(input ReportInput, nameByIndex map[int]string) []report.Tape {
	tapes := make([]report.Tape, 0, len(input.Written))

	for _, written := range input.Written {
		var contents []string

		if written.TapeIndex >= 0 && written.TapeIndex < len(input.Plan.Tapes) {
			planned := input.Plan.Tapes[written.TapeIndex]
			contents = make([]string, 0, len(planned.Archives))

			for _, placement := range planned.Archives {
				name, ok := nameByIndex[placement.SourceIndex]
				if !ok {
					name = archiveName(input.Config, placement.SourceIndex)
				}

				contents = append(contents, name)
			}
		}

		tapes = append(tapes, report.Tape{
			Barcode:           string(written.Barcode),
			Contents:          contents,
			WriteHealth:       reportWriteHealth(written.WriteHealth),
			OverwroteNonBlank: written.OverwroteNonBlank,
		})
	}

	return tapes
}

// reportWriteHealth maps a tape's observational write-health measurement to the
// report shape, returning nil when the measurement was not taken so the report can
// render "not measured" rather than a misleading zeroed row (SPEC §14).
func reportWriteHealth(health WriteHealth) *report.WriteHealth {
	if !health.Measured {
		return nil
	}

	return &report.WriteHealth{
		ThroughputMBps:      health.ThroughputMBps,
		FloorMBps:           health.FloorMBps,
		FloorKnown:          health.FloorKnown,
		BelowFloor:          health.BelowFloor,
		Repositions:         health.Repositions,
		RepositionsMeasured: health.RepositionsMeasured,
		TapeAlertFlags:      health.TapeAlertFlags,
		Healthy:             health.Healthy(),
	}
}

// buildParams records how the tapes were built (SPEC §9): tool and external tool
// versions from the committed build info, slice size and PAR2 policy from the run
// config, and the best-effort drive/library identifiers.
func buildParams(cfg config.Config, device deviceIdentity) report.BuildParams {
	return report.BuildParams{
		ToolVersion:     buildinfo.ToolVersion(),
		AgeVersion:      buildinfo.AgeVersion(),
		Par2Version:     buildinfo.Par2Version(),
		LTFSVersion:     buildinfo.LTFSVersion(),
		SliceSize:       cfg.Redundancy.SliceSizeBytes,
		PAR2Redundancy:  redundancyPolicy(cfg.Redundancy),
		DriveModel:      device.driveModel,
		DriveGeneration: device.driveGeneration,
		DriveSerial:     device.driveSerial,
		LibraryModel:    device.libraryModel,
	}
}

// redundancyPolicy renders the run's PAR2 policy for the report: the fixed target
// percentage, or fill-to-capacity with its floor (SPEC §8).
func redundancyPolicy(redundancy config.Redundancy) string {
	switch {
	case redundancy.TargetPercentage != nil:
		return fmt.Sprintf("%g%%", *redundancy.TargetPercentage)
	case redundancy.FillToCapacity != nil:
		return fmt.Sprintf("fill-to-capacity (floor %g%%)", redundancy.FillToCapacity.Floor)
	default:
		return "none"
	}
}

// buildSHA256Manifest builds the full SHA-256 manifest for the recovery ISO
// (SPEC §10): one sha256sum-format line per on-tape file across every physical
// tape, prefixed by the tape's barcode so a recoverer can verify files copied off
// any tape. Lines are sorted for a deterministic, diff-stable manifest. It reads
// no files — every digest is the precomputed one from Prepare/GeneratePAR2.
func buildSHA256Manifest(input ReportInput) ([]byte, error) {
	// resolved carries each archive's descriptive directory label, so it is needed
	// here for archivesForTape to reproduce the on-tape paths (SPEC §6).
	state := &runState{resolved: input.Resolved, staged: input.Staged, par2: input.PAR2, plan: input.Plan, written: input.Written}

	var lines []string

	for _, written := range input.Written {
		archives, err := archivesForTape(state, written.TapeIndex)
		if err != nil {
			return nil, err
		}

		manifest := buildManifest(written.Barcode, written.TapeIndex, written.CopyIndex, archives)

		for _, archiveManifest := range manifest.Archives {
			for _, file := range append(append([]ManifestFile{}, archiveManifest.Files...), archiveManifest.PAR2Files...) {
				lines = append(lines, fmt.Sprintf("%s  %s/%s", file.SHA256, written.Barcode, file.TapePath))
			}
		}
	}

	sort.Strings(lines)

	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// deviceIdentity carries the drive and library provenance rendered in the report's
// build parameters. Every field is read from the hardware by a best-effort SCSI
// INQUIRY — the model and serial directly, the drive generation from the product
// id — so nothing is hand-entered from config.
type deviceIdentity struct {
	driveModel      string
	driveSerial     string
	driveGeneration string
	libraryModel    string
}

// queryDeviceIdentity assembles the drive/library provenance for the report from a
// best-effort SCSI INQUIRY (0x12) on the first drive and the changer: the drive's
// model, serial, and — from its product id — the LTO generation required to read
// its tapes, plus the library model. INQUIRY is provenance only, so any failure
// (e.g. dry-run/mhvtl without the device) degrades each field to "unknown" and is
// logged rather than failing the run.
func queryDeviceIdentity(ctx context.Context, library config.Library) deviceIdentity {
	identity := deviceIdentity{
		driveModel:      unknownIdentity,
		driveSerial:     unknownIdentity,
		driveGeneration: unknownIdentity,
		libraryModel:    unknownIdentity,
	}

	if len(library.Drives) > 0 {
		if info, err := tape.NewDrive(library.Drives[0]).Inquire(ctx); err != nil {
			slog.Warn("report: could not read drive identity", "device", library.Drives[0], "error", err)
		} else {
			if model := info.Model(); model != "" {
				identity.driveModel = model
			}

			if info.Serial != "" {
				identity.driveSerial = info.Serial
			}

			identity.driveGeneration = info.LTOGeneration()
		}
	}

	if library.Changer != "" {
		if info, err := tape.NewChanger(library.Changer).Inquire(ctx); err != nil {
			slog.Warn("report: could not read library identity", "device", library.Changer, "error", err)
		} else if model := info.Model(); model != "" {
			identity.libraryModel = model
		}
	}

	return identity
}

// ltoGeneration maps a tape's native capacity (SPEC §5 library.tapeCapacityBytes)
// to its LTO generation, used to look up the generation's write-health
// speed-matching floor (writeHealthFloor). The report's "generation required to
// read" field instead comes from the drive's INQUIRY product id (deviceIdentity),
// not this capacity heuristic. Thresholds are the generations' native capacities;
// a capacity below LTO-5's or non-positive yields an explicit "unknown" carrying
// the raw value.
func ltoGeneration(capacityBytes int64) string {
	switch {
	case capacityBytes >= 16_000_000_000_000:
		return "LTO-9"
	case capacityBytes >= 10_000_000_000_000:
		return "LTO-8"
	case capacityBytes >= 5_000_000_000_000:
		return "LTO-7"
	case capacityBytes >= 2_000_000_000_000:
		return "LTO-6"
	case capacityBytes >= 1_200_000_000_000:
		return "LTO-5"
	default:
		return fmt.Sprintf("%s (native capacity %d bytes)", unknownIdentity, capacityBytes)
	}
}

// reportPhase orchestrates the Report phase (SPEC §4.3 phase 9): it runs the
// data-side BuildReport activity over the run state and records the artifact paths
// in runState — the PDF report for the Deliver phase, and (when optical burning is
// enabled) the uncompressed ISO and disc manifest for the Burn phase. RunID and
// Date are taken from the workflow so the artifact is stamped deterministically
// across retries.
func reportPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: reportTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
	})

	var activities *ReportActivities

	// Capture the report date once so the post-burn re-render of the delivered
	// report carries the same date as the on-disc copy that predates the burn.
	state.reportDate = workflow.Now(ctx)

	var output ReportOutput
	if err := workflow.ExecuteActivity(dataCtx, activities.BuildReport, reportInput(ctx, cfg, state)).Get(dataCtx, &output); err != nil {
		return err
	}

	state.reportPath = output.ReportPath
	state.uncompressedISOPath = output.UncompressedISOPath
	state.discManifestPath = output.DiscManifestPath

	return nil
}

// reportInput assembles the Report activity payload from the run config and run
// state (SPEC §4.3 phase 9). It is shared by the Report phase and the post-burn
// report re-render so both render from identical run state; RunID and the report
// date come from the workflow (deterministic) so the artifact is stamped
// identically across retries and across the two renders. Discs is left empty; the
// re-render sets it from the burned discs.
func reportInput(ctx workflow.Context, cfg config.Config, state *runState) ReportInput {
	return ReportInput{
		Config:   cfg,
		RunID:    workflow.GetInfo(ctx).WorkflowExecution.ID,
		Date:     state.reportDate,
		Resolved: state.resolved,
		Staged:   state.staged,
		PAR2:     state.par2,
		Plan:     state.plan,
		Written:  state.written,
	}
}
