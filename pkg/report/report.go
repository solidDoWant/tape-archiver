// Package report builds the per-run PDF report (SPEC §9): the durable, printable,
// laminated offline index for a backup run. Build renders a complete run
// Manifest into a valid PDF carrying every field SPEC §9 enumerates — run id and
// date, the full contents manifest (archives, member volumes, source snapshots,
// sizes, SHA-256 checksums), which physical tape barcodes hold what, the build
// parameters (tool/age/par2/ltfs versions, slice size, PAR2 redundancy,
// drive/library identifiers), the recovery procedure, and the age private
// identity.
//
// The report INTENTIONALLY contains the age private identity (SPEC §7 key-escrow
// decision): the holder of the printed report (or the recovery ISO that embeds
// it) can always decrypt the archives. This is the documented behavior, not a
// leak — under the personal cold-storage threat model the report and ISO carry
// the decryption secret and must be handled accordingly.
//
// Rendering uses the pure-Go github.com/go-pdf/fpdf library so the build stays
// hermetic with no runtime tool dependency, consistent with the 20-year
// recoverability principle (SPEC §2).
package report

import (
	"fmt"
	"io"
	"time"

	"github.com/go-pdf/fpdf"
)

// Manifest is the complete description of a run rendered into the PDF report. It
// is assembled by the workflow report phase and is the sole input to Build.
type Manifest struct {
	// RunID is the unique identifier of the backup run.
	RunID string
	// Date is when the run was produced.
	Date time.Time
	// Archives is the full contents manifest: one entry per archived group or
	// volume, with its member volumes, source snapshots, and per-file sizes and
	// checksums.
	Archives []Archive
	// Tapes maps each physical tape (by barcode) to what it holds.
	Tapes []Tape
	// Build records how the tapes were built: tool and external tool versions,
	// slice size, PAR2 redundancy, and drive/library identifiers.
	Build BuildParams
	// AgeIdentity is the age private identity (AGE-SECRET-KEY-PQ-1…) included
	// for key escrow (SPEC §7). See the package doc: this is deliberate.
	AgeIdentity string
	// RecoveryProcedure is the human-readable, step-by-step recovery text.
	RecoveryProcedure string
}

// Archive describes one archived group or volume in the contents manifest.
type Archive struct {
	// Name identifies the archive (the snapshot group or volume name).
	Name string
	// MemberVolumes lists the volumes contained in this archive. A snapshot
	// group is archived as a single tar with one member per volume.
	MemberVolumes []string
	// SourceSnapshots lists the source snapshot(s) the archive was built from.
	SourceSnapshots []string
	// Files are the on-tape files for this archive, each with its size and
	// SHA-256 checksum.
	Files []ArchiveFile
}

// ArchiveFile is a single on-tape file with its size and checksum.
type ArchiveFile struct {
	// Name is the file name as stored on tape.
	Name string
	// Size is the file size in bytes.
	Size int64
	// SHA256 is the lowercase hex-encoded SHA-256 digest of the file.
	SHA256 string
}

// Tape records which archives/files a single physical tape holds, referenced by
// the library-read barcode (the canonical physical ID, SPEC §6).
type Tape struct {
	// Barcode is the tape's library barcode / LTFS volume name.
	Barcode string
	// Contents lists what this tape holds (archive or file names).
	Contents []string
}

// BuildParams records how the tapes were built — the versions and settings a
// future recoverer needs to reproduce or understand the on-tape layout.
type BuildParams struct {
	// ToolVersion is the tape-archiver version that produced the run.
	ToolVersion string
	// AgeVersion is the age binary version used for encryption.
	AgeVersion string
	// Par2Version is the par2 binary version used for the recovery sets.
	Par2Version string
	// LTFSVersion is the ltfs/mkltfs version used for the on-tape filesystem.
	LTFSVersion string
	// SliceSize is the fixed slice size, in bytes, of the encrypted stream.
	SliceSize int64
	// PAR2Redundancy is the PAR2 redundancy policy as rendered (e.g. "10%" or
	// "fill-to-capacity").
	PAR2Redundancy string
	// DriveIdentifier identifies the tape drive used.
	DriveIdentifier string
	// LibraryIdentifier identifies the tape library / changer used.
	LibraryIdentifier string
}

// page layout constants (A4, millimetres).
const (
	leftMargin   = 15.0
	topMargin    = 15.0
	rightMargin  = 15.0
	contentWidth = 210.0 - leftMargin - rightMargin

	fontFamily = "Helvetica"
)

// Build renders m into a PDF report and writes it to w. It returns a non-nil
// error if the PDF cannot be generated or written. The output contains every
// SPEC §9 field, including the age private identity (see the package doc).
func Build(m Manifest, w io.Writer) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(leftMargin, topMargin, rightMargin)
	pdf.SetAutoPageBreak(true, topMargin)
	pdf.AddPage()

	title(pdf, "tape-archiver run report")

	section(pdf, "Run")
	field(pdf, "Run ID", m.RunID)
	field(pdf, "Date", m.Date.Format(time.RFC3339))

	section(pdf, "Contents manifest")

	for _, archive := range m.Archives {
		archiveBlock(pdf, archive)
	}

	section(pdf, "Tapes")

	for _, tape := range m.Tapes {
		field(pdf, "Barcode", tape.Barcode)

		for _, content := range tape.Contents {
			bullet(pdf, content)
		}
	}

	section(pdf, "Build parameters")
	field(pdf, "Tool version", m.Build.ToolVersion)
	field(pdf, "age version", m.Build.AgeVersion)
	field(pdf, "par2 version", m.Build.Par2Version)
	field(pdf, "ltfs version", m.Build.LTFSVersion)
	field(pdf, "Slice size", fmt.Sprintf("%d bytes", m.Build.SliceSize))
	field(pdf, "PAR2 redundancy", m.Build.PAR2Redundancy)
	field(pdf, "Drive identifier", m.Build.DriveIdentifier)
	field(pdf, "Library identifier", m.Build.LibraryIdentifier)

	// The age private identity is included deliberately for key escrow
	// (SPEC §7). See the package doc — this is not a leak.
	section(pdf, "age private identity (key escrow)")
	body(pdf, m.AgeIdentity)

	section(pdf, "Recovery procedure")
	body(pdf, m.RecoveryProcedure)

	if err := pdf.Output(w); err != nil {
		return fmt.Errorf("report: writing PDF: %w", err)
	}

	return nil
}

// title renders the report's top-level heading.
func title(pdf *fpdf.Fpdf, text string) {
	pdf.SetFont(fontFamily, "B", 18)
	pdf.MultiCell(contentWidth, 9, text, "", "L", false)
	pdf.Ln(2)
}

// section renders a section heading.
func section(pdf *fpdf.Fpdf, text string) {
	pdf.Ln(3)
	pdf.SetFont(fontFamily, "B", 13)
	pdf.MultiCell(contentWidth, 7, text, "", "L", false)
}

// field renders a "label: value" line. The value wraps across lines if needed.
func field(pdf *fpdf.Fpdf, label, value string) {
	pdf.SetFont(fontFamily, "B", 10)
	pdf.MultiCell(contentWidth, 5, label+":", "", "L", false)
	pdf.SetFont(fontFamily, "", 10)
	pdf.MultiCell(contentWidth, 5, value, "", "L", false)
}

// archiveBlock renders one archive and its files.
func archiveBlock(pdf *fpdf.Fpdf, archive Archive) {
	pdf.Ln(1)
	pdf.SetFont(fontFamily, "B", 11)
	pdf.MultiCell(contentWidth, 6, "Archive: "+archive.Name, "", "L", false)

	field(pdf, "Member volumes", joinOrNone(archive.MemberVolumes))
	field(pdf, "Source snapshots", joinOrNone(archive.SourceSnapshots))

	pdf.SetFont(fontFamily, "B", 10)
	pdf.MultiCell(contentWidth, 5, "Files:", "", "L", false)

	for _, file := range archive.Files {
		bullet(pdf, fmt.Sprintf("%s — %d bytes — sha256:%s", file.Name, file.Size, file.SHA256))
	}
}

// bullet renders an indented bullet line that wraps cleanly.
func bullet(pdf *fpdf.Fpdf, text string) {
	pdf.SetFont(fontFamily, "", 10)
	pdf.SetX(leftMargin + 5)
	pdf.MultiCell(contentWidth-5, 5, "- "+text, "", "L", false)
}

// body renders a free-text block (multi-line, wrapping).
func body(pdf *fpdf.Fpdf, text string) {
	pdf.SetFont(fontFamily, "", 10)
	pdf.MultiCell(contentWidth, 5, text, "", "L", false)
}

// joinOrNone joins items with ", ", returning "(none)" for an empty slice so the
// field always renders a value.
func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}

	result := items[0]
	for _, item := range items[1:] {
		result += ", " + item
	}

	return result
}
