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

// The Report phase (SPEC §4.3 phase 9) builds the PDF report and, only when
// optical burning is enabled, the recovery ISO. It runs on the data worker, where
// the staged files and recovery binaries live, and first enforces the key-escrow
// contract (SPEC §7).

const (
	reportTimeout = 1 * time.Hour

	reportFileName = "report.pdf"
	isoFileName    = "recovery.iso"
	// discManifestFileName lists the ISO's own files; the Burn phase verifies
	// each disc against it.
	discManifestFileName = "disc-manifest.sha256"

	unknownIdentity = "unknown"
)

// recoveryProcedure is the concise recovery text printed in the report so the
// printout is self-contained if the disc is lost; the full procedure ships on the
// disc as recovery-procedure.md.
const recoveryProcedure = `Tape Archiver — recovery procedure (concise)

You need only this disc and the physical tapes. All tools referenced below ship
statically linked under bin/ on this disc; the age private identity is printed in
report.pdf (and in the "Encryption key" section of this text's PDF). The full
procedure — index-loss recovery and failure handling — is in recovery-procedure.md
on this disc.

1.  Load a tape into a standalone LTO drive that can read the tape generation named
    in the report's build parameters. An LTO drive reads its own generation and the
    one before it; LTO-7 and earlier also read two generations back. A newer drive is
    therefore not automatically enough — an LTO-8 drive, for instance, reads neither
    LTO-6 nor LTO-5 media.
2.  Mount the tape's LTFS volume read-only. LTFS addresses the drive by its SCSI
    generic node (/dev/sgN — "lsscsi -g" lists it beside each tape drive); the
    /dev/nstN tape node does not work here:
        ltfs -o ro -o devname=/dev/sgN <mountpoint>
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
    (Do not add -p. A repaired slice's damaged original is kept as archive.NNN.1;
    leave it out of step 5.)
5.  Concatenate an archive's slices (archive.NNN only) in order to reconstruct its
    age stream:
        cat <barcode>/archives/NNN-<label>/archive.000 <barcode>/archives/NNN-<label>/archive.001 ... > archive.age
6.  Decrypt with the escrowed identity (save it to identity.txt first):
        bin/age -d -i identity.txt -o archive.tar.zst archive.age
7.  The contents manifest records, per archive, whether it was compressed. If this
    one was, decompress it:
        bin/zstd -d archive.tar.zst -o archive.tar
    (An archive the manifest records as uncompressed is already a tar after step 6.)
8.  Unpack the tar:
        bin/tar -xf archive.tar
    An archive holding several member volumes unpacks to one subdirectory per volume.
    A single-volume archive unpacks that volume's own contents directly, with no
    wrapping directory — the report's contents manifest names its member volume.
9.  Repeat for every archive on every tape listed in the report.`

// ReportActivities hosts the data-side Report activities.
type ReportActivities struct {
	stagingRoot string
	binariesDir string
	sourcesDir  string
}

// newReportActivities returns the production data-side Report activity.
func newReportActivities(stagingRoot, binariesDir, sourcesDir string) *ReportActivities {
	return &ReportActivities{stagingRoot: stagingRoot, binariesDir: binariesDir, sourcesDir: sourcesDir}
}

// ReportInput is the payload for the Report activity. RunID and Date come from the
// workflow so retries stamp the artifact identically.
type ReportInput struct {
	Config         config.Config
	DryRun         bool
	RunID          string
	Date           time.Time
	SliceSizeBytes int64
	Resolved       []ResolvedArchive
	Staged         []StagedArchive
	PAR2           []PAR2Set
	Plan           TapePlan
	Written        []WrittenTape
	// Discs is set only for the post-burn re-render of the delivered report.
	Discs []BurnResult
}

// ReportOutput is the Report activity's result. The ISO and disc-manifest paths
// are empty when optical burning is disabled.
type ReportOutput struct {
	ReportPath          string
	UncompressedISOPath string
	DiscManifestPath    string
}

// BuildReport builds the PDF report and, when burning is enabled, the recovery
// ISO. It fails if the escrow identity is missing or matches no recipient.
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

	var output ReportOutput

	err := withActivityHeartbeat(ctx, func() error {
		var err error

		output, err = a.buildReport(ctx, outDir, input)

		return err
	})

	return output, err
}

// buildReport is the body of BuildReport, testable without an activity context.
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

	slog.InfoContext(ctx, "report: escrow identity verified; building recovery artifacts",
		"tapes", len(input.Written), "opticalBurn", input.Config.Delivery.OpticalBurn.Enabled())

	pdf, err := renderReportPDF(ctx, input)
	if err != nil {
		return ReportOutput{}, err
	}

	reportPath := filepath.Join(outDir, reportFileName)
	if err := os.WriteFile(reportPath, pdf, 0o644); err != nil {
		return ReportOutput{}, fmt.Errorf("write PDF report to %q: %w", reportPath, err)
	}

	// The burned disc is the ISO's only durable home, so no burn means no ISO.
	if !input.Config.Delivery.OpticalBurn.Enabled() {
		slog.InfoContext(ctx, "report: built recovery artifacts", "report", reportPath)

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

	discManifestPath := filepath.Join(outDir, discManifestFileName)
	if err := os.WriteFile(discManifestPath, discManifest.Bytes(), 0o644); err != nil {
		return ReportOutput{}, fmt.Errorf("write disc-content manifest to %q: %w", discManifestPath, err)
	}

	slog.InfoContext(ctx, "report: built recovery artifacts", "report", reportPath, "iso", uncompressedISOPath)

	return ReportOutput{
		ReportPath:          reportPath,
		UncompressedISOPath: uncompressedISOPath,
		DiscManifestPath:    discManifestPath,
	}, nil
}

// renderReportPDF renders the run's PDF report to bytes (SPEC §9).
func renderReportPDF(ctx context.Context, input ReportInput) ([]byte, error) {
	manifest := buildReportManifest(input, queryDeviceIdentity(ctx, input.Config.Library))

	var pdf bytes.Buffer
	if err := report.Build(manifest, &pdf); err != nil {
		return nil, fmt.Errorf("build PDF report: %w", err)
	}

	return pdf.Bytes(), nil
}

// RebuildDeliveredReport re-renders the staged report.pdf after the Burn phase so
// it records the burned discs; the ISO is left as-is.
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

// rebuildReport is the body of RebuildDeliveredReport.
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

	slog.InfoContext(ctx, "report: re-rendered the delivered report to record the burned discs",
		"report", reportPath, "discs", len(input.Discs))

	return reportPath, nil
}

// verifyEscrowIdentity enforces the key-escrow contract (SPEC §7): the identity
// must be present and derive to one of the configured recipients.
func verifyEscrowIdentity(ctx context.Context, encryption config.Encryption) error {
	if strings.TrimSpace(encryption.Identity) == "" {
		// Deterministic misconfiguration: non-retryable so the run fails fast.
		return temporal.NewNonRetryableApplicationError(
			"encryption.identity is empty; the age private identity must be provided to escrow into the report and ISO (SPEC §7)",
			"escrow-identity-invalid",
			nil,
		)
	}

	derived, err := agewrap.RecipientFromIdentity(ctx, encryption.Identity)
	if err != nil {
		// Retryable: a transient age-keygen exec failure.
		return fmt.Errorf("derive recipient from the escrowed identity: %w", err)
	}

	if !slices.Contains(encryption.Recipients, derived) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("the escrowed identity's public key (%s) is not among the configured encryption.recipients; it could not decrypt the archives", derived),
			"escrow-identity-invalid",
			nil,
		)
	}

	return nil
}

// buildRecoveryISO writes the recovery ISO to isoPath and returns its
// disc-content manifest.
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

// tapeIndexes reads each written tape's staged LTFS index for the ISO. Indexes are
// read from disk rather than carried in the payload, which they would bloat (#221).
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
			// Compression is per source, so the recoverer needs it per archive (#340).
			Compressed: resolved.Compression,
			Files:      archiveFiles(staged, par2ByIndex[staged.SourceIndex]),
		})
	}

	return report.Manifest{
		RunID:             input.RunID,
		Date:              input.Date,
		Archives:          archives,
		Tapes:             reportTapes(input, nameByIndex),
		Discs:             reportDiscs(input.Discs),
		Build:             buildParams(input.Config, input.SliceSizeBytes, device, input.Written),
		AgeIdentity:       input.Config.Encryption.Identity,
		RecoveryProcedure: recoveryProcedure,
		DryRun:            input.DryRun,
	}
}

// reportDiscs maps the burned discs to the report shape; nil omits the section.
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

// archiveName is the display name of the archive for a source index.
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

// memberVolumes lists the volume identity of each snapshot in the archive.
func memberVolumes(resolved ResolvedArchive) []string {
	volumes := make([]string, 0, len(resolved.Snapshots))
	for _, snapshot := range resolved.Snapshots {
		volumes = append(volumes, memberName(snapshot))
	}

	return volumes
}

// sourceSnapshots lists the ZFS snapshot path of each snapshot in the archive,
// noting when a run-scoped one was taken.
func sourceSnapshots(resolved ResolvedArchive) []string {
	snapshots := make([]string, 0, len(resolved.Snapshots))
	for _, snapshot := range resolved.Snapshots {
		if snapshot.RunScoped {
			snapshots = append(snapshots, fmt.Sprintf("%s (taken by this run at %s; destroyed after it)",
				snapshot.ZFSPath, snapshot.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC")))

			continue
		}

		snapshots = append(snapshots, snapshot.ZFSPath)
	}

	return snapshots
}

// archiveFiles lists an archive's slices then PAR2 files by on-tape base name.
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

// reportTapes maps each written tape to the archive names it holds.
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

// reportWriteHealth maps write health to the report shape; nil renders as "not
// measured" rather than a misleading zeroed row.
func reportWriteHealth(health WriteHealth) *report.WriteHealth {
	if !health.Measured {
		return nil
	}

	return &report.WriteHealth{
		ThroughputMBps:         health.ThroughputMBps,
		ThroughputUnmeasurable: health.ThroughputUnmeasurable,
		FloorMBps:              health.FloorMBps,
		FloorKnown:             health.FloorKnown,
		BelowFloor:             health.BelowFloor,
		Repositions:            health.Repositions,
		RepositionsMeasured:    health.RepositionsMeasured,
		TapeAlertFlags:         health.TapeAlertFlags,
		Healthy:                health.Healthy(),
	}
}

// buildParams records how the tapes were built (SPEC §9).
func buildParams(cfg config.Config, sliceSizeBytes int64, device deviceIdentity, written []WrittenTape) report.BuildParams {
	return report.BuildParams{
		ToolVersion:     buildinfo.ToolVersion(),
		AgeVersion:      buildinfo.AgeVersion(),
		Par2Version:     buildinfo.Par2Version(),
		LTFSVersion:     buildinfo.LTFSVersion(),
		SliceSize:       sliceSizeBytes,
		PAR2Redundancy:  redundancyPolicy(cfg.Redundancy),
		TapeGeneration:  tapeGeneration(written),
		DriveModel:      device.driveModel,
		DriveGeneration: device.driveGeneration,
		DriveSerial:     device.driveSerial,
		LibraryModel:    device.libraryModel,
	}
}

// tapeGeneration reduces the per-tape media generations to one report value, or
// "mixed"/"unknown". It is never guessed from the writing drive, which may be newer
// than the media (issue #340).
func tapeGeneration(written []WrittenTape) string {
	generation := ""

	for _, tape := range written {
		switch {
		case tape.MediaGeneration == "" || tape.MediaGeneration == unknownIdentity:
			return unknownIdentity
		case generation == "":
			generation = tape.MediaGeneration
		case generation != tape.MediaGeneration:
			return "mixed"
		}
	}

	if generation == "" {
		return unknownIdentity
	}

	return generation
}

// redundancyPolicy renders the run's PAR2 policy for the report.
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

// buildSHA256Manifest builds the sorted, barcode-prefixed sha256sum manifest of
// every on-tape file from the precomputed digests.
func buildSHA256Manifest(input ReportInput) ([]byte, error) {
	// resolved carries the directory labels archivesForTape needs for tape paths.
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

// deviceIdentity is the drive/library provenance read by SCSI INQUIRY.
type deviceIdentity struct {
	driveModel      string
	driveSerial     string
	driveGeneration string
	libraryModel    string
}

// queryDeviceIdentity INQUIREs the first drive and the changer. It is provenance
// only, so failures degrade fields to "unknown" rather than failing the run.
func queryDeviceIdentity(ctx context.Context, library config.Library) deviceIdentity {
	identity := deviceIdentity{
		driveModel:      unknownIdentity,
		driveSerial:     unknownIdentity,
		driveGeneration: unknownIdentity,
		libraryModel:    unknownIdentity,
	}

	if len(library.Drives) > 0 {
		if info, err := tape.NewDrive(library.Drives[0]).Inquire(ctx); err != nil {
			slog.WarnContext(ctx, "report: could not read drive identity", "device", library.Drives[0], "error", err)
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
			slog.WarnContext(ctx, "report: could not read library identity", "device", library.Changer, "error", err)
		} else if model := info.Model(); model != "" {
			identity.libraryModel = model
		}
	}

	return identity
}

// ltoGeneration maps a native tape capacity to its LTO generation, for the
// write-health floor lookup only (not the report's generation field).
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

// reportPhase runs BuildReport and records the artifact paths in runState.
func reportPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: reportTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
	})

	var activities *ReportActivities

	// Captured once so the post-burn re-render carries the same date.
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

// reportInput assembles the Report activity payload. It uses the run id, not the
// workflow id, which is the constant "backup" for every run.
func reportInput(ctx workflow.Context, cfg config.Config, state *runState) ReportInput {
	return ReportInput{
		Config:         cfg,
		DryRun:         isDryRun(ctx),
		RunID:          workflow.GetInfo(ctx).WorkflowExecution.RunID,
		Date:           state.reportDate,
		SliceSizeBytes: state.sliceSizeBytes,
		Resolved:       state.resolved,
		Staged:         state.staged,
		PAR2:           state.par2,
		Plan:           state.plan,
		Written:        state.written,
	}
}
