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
	"strconv"
	"strings"
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
	// Discs lists the optical recovery discs burned for the run (SPEC §10), each
	// naming the burner it was written on and whether a non-blank disc was
	// deliberately reclaimed. It is empty when optical burning was not enabled;
	// the Discs section is then omitted entirely.
	Discs []Disc
	// Build records how the tapes were built: tool and external tool versions,
	// slice size, PAR2 redundancy, and drive/library identifiers.
	Build BuildParams
	// AgeIdentity is the age private identity (AGE-SECRET-KEY-PQ-1…) included
	// for key escrow (SPEC §7). See the package doc: this is deliberate.
	AgeIdentity string
	// RecoveryProcedure is the human-readable, step-by-step recovery text. Lines
	// are separated by newlines and rendered as individual steps.
	RecoveryProcedure string
}

// Archive describes one archived group or volume in the contents manifest.
type Archive struct {
	// Name identifies the archive (the snapshot group or volume name).
	Name string
	// Directory is the archive's on-tape directory relative to the LTFS root
	// (e.g. archives/000-photos), so an operator can locate its files on the
	// mounted volume. Empty renders the row as absent.
	Directory string
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
	// WriteHealth is the tape's observational write-health measurement (SPEC §14):
	// sustained throughput, repositions, and TapeAlert flags. It is nil when
	// write-health was not measured for the tape.
	WriteHealth *WriteHealth
	// OverwroteNonBlank is true when this tape was not blank at load and was written
	// over because the run set Library.AllowNonBlankTapes. The Tapes section annotates
	// such tapes so a deliberate, irreversible overwrite is recorded (SPEC §9).
	OverwroteNonBlank bool
}

// Disc records one optical recovery disc burned for the run (SPEC §10),
// referenced by the burner device it was written on.
type Disc struct {
	// Device is the optical burner the disc was written on (e.g. /dev/sr0),
	// recorded as provenance.
	Device string
	// OverwroteNonBlank is true when this disc was not blank and was reclaimed and
	// written over because the run set Delivery.OpticalBurn.AllowNonBlankDiscs. The
	// Discs section annotates such discs so a deliberate, irreversible overwrite is
	// recorded (SPEC §9, §10).
	OverwroteNonBlank bool
}

// WriteHealth is a tape's observational write-health measurement rendered in the
// report (SPEC §2 principle 2, §14). It never reflects run success — a tape flagged
// below-floor, with repositions, or with TapeAlert flags was still written.
type WriteHealth struct {
	// ThroughputMBps is the sustained write throughput over the tape's write window,
	// in MB/s (staged bytes / elapsed).
	ThroughputMBps float64
	// FloorMBps is the speed-matching floor the throughput was compared against,
	// derived from the tape generation. Meaningful only when FloorKnown is true.
	FloorMBps float64
	// FloorKnown is true when a speed-matching floor is known for the tape's
	// generation. When false the throughput is reported but not judged against a floor.
	FloorKnown bool
	// BelowFloor is true when the throughput fell below a known FloorMBps.
	BelowFloor bool
	// Repositions is the drive's back-hitch count (SCSI log page 0x24).
	Repositions int64
	// TapeAlertFlags are the active TapeAlert flags (SCSI log page 0x2e), if any.
	TapeAlertFlags []string
	// Healthy is true when the tape streamed cleanly: at or above the floor, with no
	// repositions and no active TapeAlert flags.
	Healthy bool
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
	// DriveModel is the tape drive product model, e.g. "IBM ULT3580-HH8".
	DriveModel string
	// DriveGeneration spells out the LTO generation required to read the tape —
	// the fact a future recoverer actually needs, e.g.
	// "LTO-8 (reads LTO-7/8; LTO-9 also reads LTO-8)". The source host's device
	// node is deliberately omitted: it is runtime state of the writing machine
	// and is meaningless on the (different) recovery hardware.
	DriveGeneration string
	// DriveSerial is the drive unit serial number, recorded as provenance.
	DriveSerial string
	// LibraryModel is the tape library / changer model. It is provenance only:
	// recovery loads a single cartridge into a standalone drive and does not
	// require the original autochanger.
	LibraryModel string
}

// page layout constants (A4, millimetres).
const (
	marginX     = 16.0
	marginY     = 16.0
	contentW    = 210.0 - 2*marginX // usable width
	labelColW   = 46.0              // key column width in key/value rows
	footerInset = 12.0

	// minSectionBody is the height reserved after a section bar so the heading
	// stays with the first lines of its content across a page break.
	minSectionBody = 14.0
	// minArchiveBlock keeps an archive name with its first metadata and table
	// rows rather than stranding the name at a page bottom.
	minArchiveBlock = 34.0

	fontBody = "Helvetica"
	fontMono = "Courier" // checksums, identities, device nodes
)

// palette — muted, printer-friendly tones with strong contrast for lamination.
var (
	colInk    = [3]int{33, 37, 41}    // near-black body text
	colMuted  = [3]int{108, 117, 125} // labels, captions
	colRule   = [3]int{206, 212, 218} // hairline table rules
	colBar    = [3]int{233, 236, 239} // section header / table header fill
	colKeyBG  = [3]int{255, 243, 205} // key-escrow callout fill (amber)
	colKeyBdr = [3]int{255, 193, 7}   // key-escrow callout border
)

// Build renders m into a PDF report and writes it to w. It returns a non-nil
// error if the PDF cannot be generated or written. The output contains every
// SPEC §9 field, including the age private identity (see the package doc).
func Build(m Manifest, w io.Writer) error {
	d := newDoc()

	d.title()
	d.runSection(m)
	d.contentsSection(m)
	d.tapesSection(m)
	d.discsSection(m)
	d.writeHealthSection(m)
	d.buildSection(m)
	d.identitySection(m)
	d.recoverySection(m)

	if err := d.pdf.Output(w); err != nil {
		return fmt.Errorf("report: writing PDF: %w", err)
	}

	return nil
}

// doc wraps the fpdf canvas and the cp1252 unicode translator that lets the core
// fonts render characters such as the em dash, bullet, and section sign.
type doc struct {
	pdf *fpdf.Fpdf
	tr  func(string) string
}

// newDoc constructs the canvas, installs the page footer, and opens the first
// page.
func newDoc() *doc {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(marginX, marginY, marginX)
	pdf.SetAutoPageBreak(true, marginY)
	pdf.AliasNbPages("")

	d := &doc{pdf: pdf, tr: pdf.UnicodeTranslatorFromDescriptor("")}
	pdf.SetFooterFunc(d.footer)
	pdf.AddPage()

	return d
}

// footer prints a centered "Page X of N" line on every page.
func (d *doc) footer() {
	d.pdf.SetY(-footerInset)
	d.pdf.SetFont(fontBody, "I", 8)
	d.text(colMuted)
	d.pdf.CellFormat(0, 6, d.tr(fmt.Sprintf("Page %d of {nb}", d.pdf.PageNo())), "", 0, "C", false, 0, "")
}

// title renders the report heading, subtitle, and a separating rule.
func (d *doc) title() {
	d.pdf.SetFont(fontBody, "B", 20)
	d.text(colInk)
	d.pdf.MultiCell(contentW, 9, d.tr("Tape Archiver — Run Report"), "", "L", false)

	d.pdf.SetFont(fontBody, "I", 9.5)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5, d.tr("Durable offline index — print, laminate, and store with the physical tapes."), "", "L", false)

	d.pdf.Ln(2.5)
	d.draw(colInk)
	d.pdf.SetLineWidth(0.4)
	y := d.pdf.GetY()
	d.pdf.Line(marginX, y, marginX+contentW, y)
}

// ensureSpace forces a page break when fewer than h millimetres of usable height
// remain on the current page. It is used to keep a heading on the same page as
// the content that follows it.
func (d *doc) ensureSpace(h float64) {
	_, pageH := d.pdf.GetPageSize()
	_, _, _, bottom := d.pdf.GetMargins()

	if d.pdf.GetY()+h > pageH-bottom {
		d.pdf.AddPage()
	}
}

// rowCell is one cell of a table row rendered by tableRow.
type rowCell struct {
	w     float64 // column width
	text  string  // cell text (may wrap across lines)
	align string  // "L" (default) or "R"
	mono  bool    // monospace font (barcodes, device nodes)
}

// tableRow renders one table row as an atomic unit. It measures the tallest cell,
// and if the row will not fit on the current page it moves the whole row to the
// next page — re-drawing the column header via drawHeader — before drawing any
// cell, so a row's columns can never split across a page boundary. Every cell is
// then drawn from the same top and a hairline rule closes the row.
//
// This is deliberately not built from a MultiCell followed by SetXY back to the
// row's starting y: a MultiCell that auto-page-breaks mid-row would leave the
// later, manually positioned columns anchored to the previous page's y, which
// scatters a single row's columns across pages.
func (d *doc) tableRow(cells []rowCell, lineH float64, drawHeader func()) {
	rowH := lineH

	for _, cell := range cells {
		lines := d.pdf.SplitLines([]byte(d.tr(cell.text)), cell.w)
		if h := float64(len(lines)) * lineH; h > rowH {
			rowH = h
		}
	}

	_, pageH := d.pdf.GetPageSize()
	_, _, _, bottom := d.pdf.GetMargins()

	if d.pdf.GetY()+rowH > pageH-bottom {
		d.pdf.AddPage()

		if drawHeader != nil {
			drawHeader()
		}
	}

	x := marginX
	y := d.pdf.GetY()

	d.text(colInk)

	for _, cell := range cells {
		align := cell.align
		if align == "" {
			align = "L"
		}

		if cell.mono {
			d.pdf.SetFont(fontMono, "", 8.5)
		} else {
			d.pdf.SetFont(fontBody, "", 8.5)
		}

		d.pdf.SetXY(x, y)
		d.pdf.MultiCell(cell.w, lineH, d.tr(cell.text), "", align, false)

		x += cell.w
	}

	d.draw(colRule)
	d.pdf.SetLineWidth(0.2)

	rowBottom := y + rowH
	d.pdf.Line(marginX, rowBottom, marginX+contentW, rowBottom)
	d.pdf.SetXY(marginX, rowBottom)
}

// section renders a full-width filled heading bar. It first reserves enough
// vertical space for the bar plus the first lines of its content, so a heading
// is never stranded at the bottom of a page apart from what it introduces.
func (d *doc) section(title string) {
	const headerHeight = 5 + 8 + 2 // top gap + bar + bottom gap

	d.ensureSpace(headerHeight + minSectionBody)

	d.pdf.Ln(5)
	d.fill(colBar)
	d.text(colInk)
	d.pdf.SetFont(fontBody, "B", 12)
	d.pdf.CellFormat(contentW, 8, d.tr("  "+title), "", 1, "L", true, 0, "")
	d.pdf.Ln(2)
}

// kv renders a "label / value" row, wrapping the value as needed and keeping the
// label top-aligned. When mono is true the value uses the monospace font (for
// identifiers, device nodes, and other fixed-width data).
func (d *doc) kv(label, value string, mono bool) {
	// Measure the row and force a page break before capturing x,y so the label
	// and value can never split across a page boundary. This mirrors tableRow:
	// without the guard the label's MultiCell auto-page-breaks when the row
	// lands with too little space left, but the value is then positioned with
	// SetXY back to the pre-break y, stranding it on a different page from its
	// label. SplitLines measures under each cell's own font, so set the font
	// before measuring.
	d.pdf.SetFont(fontBody, "B", 10)
	labelLines := d.pdf.SplitLines([]byte(d.tr(label)), labelColW)

	if mono {
		d.pdf.SetFont(fontMono, "", 9.5)
	} else {
		d.pdf.SetFont(fontBody, "", 10)
	}

	valueLines := d.pdf.SplitLines([]byte(d.tr(value)), contentW-labelColW)

	rowH := float64(max(len(labelLines), len(valueLines))) * 5.5

	_, pageH := d.pdf.GetPageSize()
	_, _, _, bottom := d.pdf.GetMargins()

	if d.pdf.GetY()+rowH > pageH-bottom {
		d.pdf.AddPage()
	}

	x, y := d.pdf.GetX(), d.pdf.GetY()

	d.pdf.SetFont(fontBody, "B", 10)
	d.text(colMuted)
	d.pdf.MultiCell(labelColW, 5.5, d.tr(label), "", "L", false)
	labelEndY := d.pdf.GetY()

	d.pdf.SetXY(x+labelColW, y)

	if mono {
		d.pdf.SetFont(fontMono, "", 9.5)
	} else {
		d.pdf.SetFont(fontBody, "", 10)
	}

	d.text(colInk)
	d.pdf.MultiCell(contentW-labelColW, 5.5, d.tr(value), "", "L", false)

	if d.pdf.GetY() < labelEndY {
		d.pdf.SetY(labelEndY)
	}

	d.pdf.SetX(x)
}

// runSection renders the run id and date.
func (d *doc) runSection(m Manifest) {
	d.section("Run")
	d.kv("Run ID", m.RunID, true)
	d.kv("Date", m.Date.Format(time.RFC3339), false)
}

// contentsSection renders the contents manifest: one block per archive.
func (d *doc) contentsSection(m Manifest) {
	d.section("Contents manifest")

	for i, archive := range m.Archives {
		if i > 0 {
			d.pdf.Ln(3)
		}

		d.archive(archive)
	}
}

// archive renders a single archive's metadata and its files table.
func (d *doc) archive(archive Archive) {
	d.ensureSpace(minArchiveBlock)

	d.pdf.SetFont(fontBody, "B", 11)
	d.text(colInk)
	d.pdf.MultiCell(contentW, 6, d.tr(archive.Name), "", "L", false)
	d.pdf.Ln(0.5)

	if archive.Directory != "" {
		d.kv("On-tape directory", archive.Directory, false)
	}

	d.kv("Member volumes", joinOrNone(archive.MemberVolumes), false)
	d.kv("Source snapshots", joinOrNone(archive.SourceSnapshots), false)
	d.pdf.Ln(1.5)

	d.filesTable(archive.Files)
}

// filesTable renders the per-file name, size, and checksum as a table so each
// file's SHA-256 sits on the same row as its name rather than on a separate
// line.
func (d *doc) filesTable(files []ArchiveFile) {
	const (
		nameW = 50.0
		sizeW = 30.0
	)

	shaW := contentW - nameW - sizeW

	d.draw(colRule)
	d.pdf.SetLineWidth(0.2)
	d.fill(colBar)
	d.text(colMuted)
	d.pdf.SetFont(fontBody, "B", 8.5)
	d.pdf.CellFormat(nameW, 6, d.tr("File"), "B", 0, "L", true, 0, "")
	d.pdf.CellFormat(sizeW, 6, d.tr("Size (bytes)"), "B", 0, "R", true, 0, "")
	d.pdf.CellFormat(shaW, 6, d.tr("SHA-256"), "B", 1, "L", true, 0, "")

	for _, file := range files {
		d.text(colInk)
		d.pdf.SetFont(fontBody, "", 8.5)
		d.pdf.CellFormat(nameW, 5.5, d.tr(file.Name), "B", 0, "L", false, 0, "")
		d.pdf.CellFormat(sizeW, 5.5, d.tr(groupDigits(file.Size)), "B", 0, "R", false, 0, "")
		d.pdf.SetFont(fontMono, "", 7)
		d.pdf.CellFormat(shaW, 5.5, d.tr(file.SHA256), "B", 1, "L", false, 0, "")
	}
}

// tapesSection renders which barcode holds what, as a table.
func (d *doc) tapesSection(m Manifest) {
	d.section("Tapes")

	const barcodeW = 46.0

	holdsW := contentW - barcodeW

	header := func() {
		d.draw(colRule)
		d.pdf.SetLineWidth(0.2)
		d.fill(colBar)
		d.text(colMuted)
		d.pdf.SetFont(fontBody, "B", 8.5)
		d.pdf.CellFormat(barcodeW, 6, d.tr("Barcode"), "B", 0, "L", true, 0, "")
		d.pdf.CellFormat(holdsW, 6, d.tr("Holds"), "B", 1, "L", true, 0, "")
	}

	header()

	for _, tape := range m.Tapes {
		holds := joinOrNone(tape.Contents)
		if tape.OverwroteNonBlank {
			// The run deliberately reclaimed a used tape (Library.AllowNonBlankTapes).
			// Record the irreversible overwrite alongside the tape's contents.
			holds += "\n[Overwrote a non-blank tape]"
		}

		d.tableRow([]rowCell{
			{w: barcodeW, text: tape.Barcode, mono: true},
			{w: holdsW, text: holds},
		}, 5.5, header)
	}
}

// discsSection renders the optical recovery discs burned for the run (SPEC §10),
// as a table of burner device and notes. It is omitted entirely when no discs
// were burned (optical burning disabled), so a run without burning renders
// exactly as before.
func (d *doc) discsSection(m Manifest) {
	if len(m.Discs) == 0 {
		return
	}

	d.section("Recovery discs")

	const deviceW = 46.0

	notesW := contentW - deviceW

	header := func() {
		d.draw(colRule)
		d.pdf.SetLineWidth(0.2)
		d.fill(colBar)
		d.text(colMuted)
		d.pdf.SetFont(fontBody, "B", 8.5)
		d.pdf.CellFormat(deviceW, 6, d.tr("Burner"), "B", 0, "L", true, 0, "")
		d.pdf.CellFormat(notesW, 6, d.tr("Notes"), "B", 1, "L", true, 0, "")
	}

	header()

	for _, disc := range m.Discs {
		notes := "burned and verified"
		if disc.OverwroteNonBlank {
			// The run deliberately reclaimed a used rewritable disc
			// (Delivery.OpticalBurn.AllowNonBlankDiscs). Record the irreversible
			// overwrite alongside the disc's provenance.
			notes += "\n[Overwrote a non-blank disc]"
		}

		d.tableRow([]rowCell{
			{w: deviceW, text: disc.Device, mono: true},
			{w: notesW, text: notes},
		}, 5.5, header)
	}
}

// writeHealthSection renders the per-tape write-health measurement (SPEC §14):
// sustained throughput against the speed-matching floor, reposition count, and any
// TapeAlert flags, with a status that flags below-floor / repositions / TapeAlert.
// It is observational — a flagged tape was still written successfully.
func (d *doc) writeHealthSection(m Manifest) {
	d.section("Write health")

	d.pdf.SetFont(fontBody, "I", 9)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5,
		d.tr("Observational only (SPEC §2 principle 2): sustained throughput vs. the speed-matching floor, drive repositions, and TapeAlert flags. A flagged tape was still written successfully."),
		"", "L", false)
	d.pdf.Ln(1.5)

	const (
		barcodeW = 40.0
		thrW     = 26.0
		floorW   = 22.0
		reposW   = 22.0
	)

	statusW := contentW - barcodeW - thrW - floorW - reposW

	header := func() {
		d.draw(colRule)
		d.pdf.SetLineWidth(0.2)
		d.fill(colBar)
		d.text(colMuted)
		d.pdf.SetFont(fontBody, "B", 8.5)
		d.pdf.CellFormat(barcodeW, 6, d.tr("Barcode"), "B", 0, "L", true, 0, "")
		d.pdf.CellFormat(thrW, 6, d.tr("MB/s"), "B", 0, "R", true, 0, "")
		d.pdf.CellFormat(floorW, 6, d.tr("Floor"), "B", 0, "R", true, 0, "")
		d.pdf.CellFormat(reposW, 6, d.tr("Repos"), "B", 0, "R", true, 0, "")
		d.pdf.CellFormat(statusW, 6, d.tr("Status"), "B", 1, "L", true, 0, "")
	}

	header()

	for _, tape := range m.Tapes {
		health := tape.WriteHealth

		thr, floor, repos := "-", "-", "-"
		if health != nil {
			floor = "n/a"
			if health.FloorKnown {
				floor = fmt.Sprintf("%.0f", health.FloorMBps)
			}

			thr = fmt.Sprintf("%.1f", health.ThroughputMBps)
			repos = strconv.FormatInt(health.Repositions, 10)
		}

		d.tableRow([]rowCell{
			{w: barcodeW, text: tape.Barcode, mono: true},
			{w: thrW, text: thr, align: "R"},
			{w: floorW, text: floor, align: "R"},
			{w: reposW, text: repos, align: "R"},
			{w: statusW, text: writeHealthStatus(health)},
		}, 5.5, header)
	}
}

// writeHealthStatus renders the human-readable status for a tape's write health:
// "not measured" when absent, "healthy" when it streamed cleanly, or the joined set
// of flags (below floor / repositions / TapeAlert descriptions) otherwise.
func writeHealthStatus(health *WriteHealth) string {
	if health == nil {
		return "not measured"
	}

	if health.Healthy {
		return "healthy"
	}

	var flags []string

	switch {
	case !health.FloorKnown:
		flags = append(flags, "floor unknown for this LTO generation")
	case health.BelowFloor:
		flags = append(flags, fmt.Sprintf("below floor (%.1f < %.0f MB/s)", health.ThroughputMBps, health.FloorMBps))
	}

	if health.Repositions > 0 {
		flags = append(flags, fmt.Sprintf("%d repositions", health.Repositions))
	}

	if len(health.TapeAlertFlags) > 0 {
		flags = append(flags, "TapeAlert: "+strings.Join(health.TapeAlertFlags, "; "))
	}

	if len(flags) == 0 {
		// Not healthy but no specific flag set (e.g. throughput not measurable):
		// avoid rendering an empty status cell.
		return "measured"
	}

	return strings.Join(flags, "; ")
}

// buildSection renders the build parameters as key/value rows.
func (d *doc) buildSection(m Manifest) {
	d.section("Build parameters")

	build := m.Build
	d.kv("Tool version", build.ToolVersion, false)
	d.kv("age version", build.AgeVersion, false)
	d.kv("par2 version", build.Par2Version, false)
	d.kv("ltfs version", build.LTFSVersion, false)
	d.kv("Slice size", fmt.Sprintf("%s bytes (%s)", groupDigits(build.SliceSize), humanSize(build.SliceSize)), false)
	d.kv("PAR2 redundancy", build.PAR2Redundancy, false)
	d.kv("Drive model", build.DriveModel, false)
	d.kv("Drive generation", build.DriveGeneration, false)
	d.kv("Drive serial", build.DriveSerial, true)
	d.kv("Library model", build.LibraryModel, false)
}

// identitySection renders the age private identity inside a highlighted callout,
// with a note that its inclusion is deliberate (SPEC §7 key escrow).
func (d *doc) identitySection(m Manifest) {
	d.section("Encryption key — age private identity")

	d.pdf.SetFont(fontBody, "I", 9)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5,
		d.tr("Included deliberately for key escrow (SPEC §7). Anyone holding this report can decrypt the archives — store and dispose of it accordingly."),
		"", "L", false)
	d.pdf.Ln(2)

	d.fill(colKeyBG)
	d.draw(colKeyBdr)
	d.pdf.SetLineWidth(0.4)
	d.text(colInk)
	d.pdf.SetFont(fontMono, "", 10)
	d.pdf.MultiCell(contentW, 7, d.tr(m.AgeIdentity), "1", "L", true)
}

// recoverySection renders the recovery procedure with its structure preserved:
// a title, wrapped intro paragraphs, numbered steps with a hanging indent, and
// indented command lines in monospace. It reads the source text's leading
// indentation to classify each line rather than flattening everything to the
// left margin (which produced ragged, unindented output).
func (d *doc) recoverySection(m Manifest) {
	d.section("Recovery procedure")

	const (
		stepNumW  = 7.0 // gutter holding the step number
		cmdIndent = 6.0 // extra indent for command lines under a step body
		lineH     = 5.0
	)

	titleDone := false

	// Re-flow the source text: the procedure is hard-wrapped at ~80 columns, so a
	// prose paragraph arrives as several lines. Joining each paragraph back into
	// one line lets MultiCell wrap it to the page naturally instead of preserving
	// the source's arbitrary break points mid-sentence. Command lines are left
	// untouched, one per line.
	lines := normalizeRecovery(m.RecoveryProcedure)

	for i := 0; i < len(lines); i++ {
		raw := lines[i]

		content := strings.TrimSpace(raw)
		if content == "" {
			d.pdf.Ln(2) // blank line -> paragraph gap
			continue
		}

		indent := leadingSpaces(raw)
		number, rest := splitStep(content)

		switch {
		case !titleDone:
			// The first non-empty line is the procedure's title.
			titleDone = true

			d.pdf.SetFont(fontBody, "B", 11)
			d.text(colInk)
			d.pdf.SetX(marginX)
			d.pdf.MultiCell(contentW, 6, d.tr(content), "", "L", false)
			d.pdf.Ln(0.5)

		case indent == 0 && number != "":
			// "N. text" -> the number in the gutter, the text hanging-indented.
			// Reserve space for the whole step block — the step line plus the
			// indented command/continuation lines that belong to it (up to the
			// next step, blank line, or paragraph) — so a page break never lands
			// between an instruction and its command.
			d.ensureSpace(d.stepBlockHeight(lines, i, stepNumW, cmdIndent, lineH))
			y := d.pdf.GetY()

			d.pdf.SetFont(fontBody, "B", 10)
			d.text(colMuted)
			d.pdf.SetXY(marginX, y)
			d.pdf.CellFormat(stepNumW, lineH, d.tr(number), "", 0, "L", false, 0, "")

			d.pdf.SetFont(fontBody, "", 10)
			d.text(colInk)
			d.pdf.SetXY(marginX+stepNumW, y)
			d.pdf.MultiCell(contentW-stepNumW, lineH, d.tr(rest), "", "L", false)
			d.pdf.SetX(marginX)

		case indent >= 8:
			// Deeply indented command line -> monospace, under the step body.
			d.pdf.SetFont(fontMono, "", 8.5)
			d.text(colInk)
			d.pdf.SetX(marginX + stepNumW + cmdIndent)
			d.pdf.MultiCell(contentW-stepNumW-cmdIndent, lineH, d.tr(content), "", "L", false)
			d.pdf.SetX(marginX)

		case indent >= 1:
			// Step continuation prose -> aligned to the step body column.
			d.pdf.SetFont(fontBody, "", 10)
			d.text(colInk)
			d.pdf.SetX(marginX + stepNumW)
			d.pdf.MultiCell(contentW-stepNumW, lineH, d.tr(content), "", "L", false)
			d.pdf.SetX(marginX)

		default:
			// An indent-0 line that is not a numbered step -> a full-width
			// paragraph (the intro text).
			d.pdf.SetFont(fontBody, "", 10)
			d.text(colInk)
			d.pdf.SetX(marginX)
			d.pdf.MultiCell(contentW, lineH, d.tr(content), "", "L", false)
		}
	}
}

// splitStep splits a "N.  text" line into its number label ("N.") and the
// remaining text. It returns an empty number when the line is not a numbered
// step, so callers can treat it as ordinary prose.
func splitStep(s string) (number, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}

	if i == 0 || i >= len(s) || s[i] != '.' {
		return "", s
	}

	return s[:i+1], strings.TrimSpace(s[i+1:])
}

// leadingSpaces returns the number of leading space/tab characters in s.
func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// normalizeRecovery re-flows the recovery procedure so each prose paragraph is a
// single line, ready to be wrapped to the page. The source is hard-wrapped at
// ~80 columns; preserving those breaks and re-wrapping each fragment produced
// ragged, mid-sentence line breaks. It joins consecutive prose lines (a numbered
// step and the soft-wrapped continuation lines that follow it) into one line,
// preserving the paragraph's leading indent, and leaves blank lines and command
// lines (indent >= 8) on their own so they render verbatim.
func normalizeRecovery(text string) []string {
	var out []string

	var para strings.Builder

	paraIndent := 0
	titleDone := false

	flush := func() {
		if para.Len() == 0 {
			return
		}

		out = append(out, strings.Repeat(" ", paraIndent)+para.String())
		para.Reset()
	}

	for _, raw := range strings.Split(text, "\n") {
		content := strings.TrimSpace(raw)
		if content == "" {
			flush()

			out = append(out, "")

			continue
		}

		indent := leadingSpaces(raw)
		number, _ := splitStep(content)

		switch {
		case !titleDone:
			titleDone = true

			flush()

			out = append(out, content)
		case indent >= 8:
			// Command line: keep it verbatim, on its own line.
			flush()

			out = append(out, raw)
		case para.Len() > 0 && (indent != 0 || number == ""):
			// A soft-wrapped continuation of the open paragraph.
			para.WriteString(" ")
			para.WriteString(content)
		default:
			// A numbered step, or the first line of a new prose paragraph.
			flush()

			paraIndent = indent

			para.WriteString(content)
		}
	}

	flush()

	return out
}

// wrappedHeight returns the rendered height of text wrapped to width in the body
// or monospace recovery font. It sets the font it measures in (the caller resets
// the font when it draws), and never reports less than a single line.
func (d *doc) wrappedHeight(text string, width float64, mono bool, lineH float64) float64 {
	if mono {
		d.pdf.SetFont(fontMono, "", 8.5)
	} else {
		d.pdf.SetFont(fontBody, "", 10)
	}

	count := len(d.pdf.SplitLines([]byte(d.tr(text)), width))
	if count < 1 {
		count = 1
	}

	return float64(count) * lineH
}

// stepBlockHeight measures a recovery step and the indented lines that belong to
// it — its wrapped command and continuation lines, up to the next step, blank
// line, or paragraph — so the whole block can be kept together on one page.
func (d *doc) stepBlockHeight(lines []string, start int, stepNumW, cmdIndent, lineH float64) float64 {
	_, rest := splitStep(strings.TrimSpace(lines[start]))
	height := d.wrappedHeight(rest, contentW-stepNumW, false, lineH)

	for j := start + 1; j < len(lines); j++ {
		indent := leadingSpaces(lines[j])
		if strings.TrimSpace(lines[j]) == "" || indent == 0 {
			break
		}

		if indent >= 8 {
			height += d.wrappedHeight(strings.TrimSpace(lines[j]), contentW-stepNumW-cmdIndent, true, lineH)
		} else {
			height += d.wrappedHeight(strings.TrimSpace(lines[j]), contentW-stepNumW, false, lineH)
		}
	}

	return height
}

// text, fill, and draw set the current text, fill, and draw colors.
func (d *doc) text(c [3]int) { d.pdf.SetTextColor(c[0], c[1], c[2]) }
func (d *doc) fill(c [3]int) { d.pdf.SetFillColor(c[0], c[1], c[2]) }
func (d *doc) draw(c [3]int) { d.pdf.SetDrawColor(c[0], c[1], c[2]) }

// joinOrNone joins items with ", ", returning "(none)" for an empty slice so the
// field always renders a value.
func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}

	return strings.Join(items, ", ")
}

// groupDigits formats n in base 10 with thousands separators, e.g. 1073741824
// becomes "1,073,741,824".
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)

	negative := strings.HasPrefix(digits, "-")
	if negative {
		digits = digits[1:]
	}

	var builder strings.Builder

	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			builder.WriteByte(',')
		}

		builder.WriteRune(digit)
	}

	if negative {
		return "-" + builder.String()
	}

	return builder.String()
}

// humanSize renders a byte count in binary units (KiB, MiB, …), e.g. 268435456
// becomes "256.00 MiB". Values below 1 KiB are rendered in bytes.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), units[exp])
}
