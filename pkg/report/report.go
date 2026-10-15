// Package report builds the per-run PDF report (SPEC §9), the printable offline
// index for a backup run.
//
// The report INTENTIONALLY contains the age private identity (SPEC §7 key
// escrow): whoever holds the report can decrypt the archives.
//
// Rendering uses pure-Go fpdf so the build has no runtime tool dependency (SPEC §2).
package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// Manifest is the complete description of a run and the sole input to Build.
type Manifest struct {
	RunID    string
	Date     time.Time
	Archives []Archive
	Tapes    []Tape
	// Discs is empty when optical burning was disabled; the section is then omitted.
	Discs []Disc
	Build BuildParams
	// AgeIdentity is deliberately included for key escrow (SPEC §7).
	AgeIdentity       string
	RecoveryProcedure string
	// DryRun marks a rehearsal against the virtual library: no real tape holds
	// this data, so every page says so.
	DryRun bool
}

// Archive describes one archived group or volume in the contents manifest.
type Archive struct {
	Name string
	// Directory is relative to the LTFS root (e.g. archives/000-photos); empty omits the row.
	Directory       string
	MemberVolumes   []string
	SourceSnapshots []string
	// Compressed records per archive whether a zstd step sits between the
	// decrypted stream and the tar, so a recoverer need not guess from magic bytes.
	Compressed bool
	Files      []ArchiveFile
}

// ArchiveFile is a single on-tape file with its size and checksum.
type ArchiveFile struct {
	Name string
	Size int64
	// SHA256 is lowercase hex.
	SHA256 string
}

// Tape records what a single physical tape holds, keyed by library barcode (SPEC §6).
type Tape struct {
	Barcode  string
	Contents []string
	// WriteHealth is nil when write health was not measured.
	WriteHealth *WriteHealth
	// OverwroteNonBlank records a deliberate overwrite under Library.AllowNonBlankTapes (SPEC §9).
	OverwroteNonBlank bool
}

// Disc records one optical recovery disc burned for the run (SPEC §10).
type Disc struct {
	Device string
	// OverwroteNonBlank records a deliberate overwrite under
	// Delivery.OpticalBurn.AllowNonBlankDiscs (SPEC §9, §10).
	OverwroteNonBlank bool
}

// WriteHealth is a tape's observational write-health measurement (SPEC §14). It
// never reflects run success: a flagged tape was still written.
type WriteHealth struct {
	ThroughputMBps float64
	// FloorMBps is meaningful only when FloorKnown is true.
	FloorMBps  float64
	FloorKnown bool
	BelowFloor bool
	// Repositions (log page 0x30) is meaningful only when RepositionsMeasured is
	// true; an unread counter must never be reported as a clean zero.
	Repositions         int64
	RepositionsMeasured bool
	TapeAlertFlags      []string
	Healthy             bool
}

// BuildParams records the versions and settings a future recoverer needs to
// understand the on-tape layout.
type BuildParams struct {
	ToolVersion string
	AgeVersion  string
	Par2Version string
	LTFSVersion string
	SliceSize   int64
	// PAR2Redundancy is rendered, e.g. "10%" or "fill-to-capacity".
	PAR2Redundancy string
	// TapeGeneration is the media's LTO generation, deliberately not the drive's:
	// read compatibility is bounded (an LTO-8 drive reads neither LTO-6 nor LTO-5),
	// so naming the drive could send a recoverer after hardware that cannot read
	// these tapes. "mixed" or "unknown" when not a single known generation.
	TapeGeneration string
	DriveModel     string
	// DriveGeneration is provenance only. The device node is deliberately omitted:
	// it is meaningless on the recovery hardware.
	DriveGeneration string
	DriveSerial     string
	// LibraryModel is provenance only; recovery needs just a standalone drive.
	LibraryModel string
}

// page layout constants (A4, millimetres).
const (
	marginX     = 16.0
	marginY     = 16.0
	contentW    = 210.0 - 2*marginX // usable width
	labelColW   = 46.0              // key column width in key/value rows
	footerInset = 12.0

	// Heights reserved so a heading/archive name is not stranded at a page bottom.
	minSectionBody  = 14.0
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
	colDryRun = [3]int{200, 35, 51}   // dry-run warning
)

// Build renders m as a PDF report to w, including the age private identity.
func Build(m Manifest, w io.Writer) error {
	d := newDoc(m.DryRun)

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

// doc wraps the fpdf canvas and the cp1252 translator the core fonts need for
// characters such as the em dash and section sign.
type doc struct {
	pdf    *fpdf.Fpdf
	tr     func(string) string
	dryRun bool
}

func newDoc(dryRun bool) *doc {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(marginX, marginY, marginX)
	pdf.SetAutoPageBreak(true, marginY)
	pdf.AliasNbPages("")

	d := &doc{pdf: pdf, tr: pdf.UnicodeTranslatorFromDescriptor(""), dryRun: dryRun}
	pdf.SetFooterFunc(d.footer)
	pdf.AddPage()

	return d
}

func (d *doc) footer() {
	d.pdf.SetY(-footerInset)
	d.pdf.SetFont(fontBody, "I", 8)
	d.text(colMuted)

	footer := fmt.Sprintf("Page %d of {nb}", d.pdf.PageNo())
	if d.dryRun {
		d.text(colDryRun)

		footer = "DRY RUN — no real tapes were written — " + footer
	}

	d.pdf.CellFormat(0, 6, d.tr(footer), "", 0, "C", false, 0, "")
}

func (d *doc) title() {
	d.pdf.SetFont(fontBody, "B", 20)
	d.text(colInk)
	d.pdf.MultiCell(contentW, 9, d.tr("Tape Archiver — Run Report"), "", "L", false)

	d.pdf.SetFont(fontBody, "I", 9.5)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5, d.tr("Durable offline index — print, laminate, and store with the physical tapes."), "", "L", false)

	if d.dryRun {
		d.pdf.Ln(2)
		d.pdf.SetFont(fontBody, "B", 12)
		d.text(colDryRun)
		d.pdf.MultiCell(contentW, 6, d.tr("DRY RUN — a rehearsal against the virtual (mhvtl) library. No real tapes were "+
			"written; the barcodes below are the real library's, mirrored. Do not file this report with any tapes."), "", "L", false)
	}

	d.pdf.Ln(2.5)
	d.draw(colInk)
	d.pdf.SetLineWidth(0.4)
	y := d.pdf.GetY()
	d.pdf.Line(marginX, y, marginX+contentW, y)
}

// ensureSpace breaks the page when fewer than h mm remain.
func (d *doc) ensureSpace(h float64) {
	_, pageH := d.pdf.GetPageSize()
	_, _, _, bottom := d.pdf.GetMargins()

	if d.pdf.GetY()+h > pageH-bottom {
		d.pdf.AddPage()
	}
}

type rowCell struct {
	w     float64 // column width
	text  string  // cell text (may wrap across lines)
	align string  // "L" (default) or "R"
	mono  bool    // monospace font (barcodes, device nodes)
}

// tableRow renders one row atomically: it measures the tallest cell and breaks
// the page (redrawing the header) before drawing, so a row never splits across
// pages. A MultiCell auto-break mid-row would strand later columns on the
// previous page. Each cell is measured under its own font, since SplitLines uses
// the active font and a wrong one under-counts wrapped lines.
func (d *doc) tableRow(cells []rowCell, lineH float64, drawHeader func()) {
	rowH := lineH

	for _, cell := range cells {
		d.setCellFont(cell)

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

		d.setCellFont(cell)

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

func (d *doc) setCellFont(cell rowCell) {
	if cell.mono {
		d.pdf.SetFont(fontMono, "", 8.5)
	} else {
		d.pdf.SetFont(fontBody, "", 8.5)
	}
}

// section renders a heading bar, keeping it on the same page as its first content lines.
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

// kv renders a label/value row; mono renders the value in the monospace font.
func (d *doc) kv(label, value string, mono bool) {
	// Break the page before capturing x,y so label and value never split (see tableRow).
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

func (d *doc) runSection(m Manifest) {
	d.section("Run")
	d.kv("Run ID", m.RunID, true)
	d.kv("Date", m.Date.Format(time.RFC3339), false)
}

func (d *doc) contentsSection(m Manifest) {
	d.section("Contents manifest")

	for i, archive := range m.Archives {
		if i > 0 {
			d.pdf.Ln(3)
		}

		d.archive(archive)
	}
}

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
	d.kv("Compression", compressionLabel(archive.Compressed), false)
	d.pdf.Ln(1.5)

	d.filesTable(archive.Files)
}

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
			holds += "\n[Overwrote a non-blank tape]"
		}

		d.tableRow([]rowCell{
			{w: barcodeW, text: tape.Barcode, mono: true},
			{w: holdsW, text: holds},
		}, 5.5, header)
	}
}

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
			notes += "\n[Overwrote a non-blank disc]"
		}

		d.tableRow([]rowCell{
			{w: deviceW, text: disc.Device, mono: true},
			{w: notesW, text: notes},
		}, 5.5, header)
	}
}

func (d *doc) writeHealthSection(m Manifest) {
	d.section("Write health")

	d.pdf.SetFont(fontBody, "I", 9)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5,
		d.tr("Observational only: sustained throughput vs. the speed-matching floor, drive repositions, and TapeAlert flags. A flagged tape was still written successfully."),
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

			// "n/m" keeps an unread counter from looking like a clean zero (SPEC §14).
			repos = "n/m"
			if health.RepositionsMeasured {
				repos = strconv.FormatInt(health.Repositions, 10)
			}
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

	switch {
	case !health.RepositionsMeasured:
		flags = append(flags, "reposition counter not supported")
	case health.Repositions > 0:
		flags = append(flags, fmt.Sprintf("%d repositions", health.Repositions))
	}

	if len(health.TapeAlertFlags) > 0 {
		flags = append(flags, "TapeAlert: "+strings.Join(health.TapeAlertFlags, "; "))
	}

	if len(flags) == 0 {
		return "measured"
	}

	return strings.Join(flags, "; ")
}

func (d *doc) buildSection(m Manifest) {
	d.section("Build parameters")

	build := m.Build
	d.kv("Tool version", build.ToolVersion, false)
	d.kv("age version", build.AgeVersion, false)
	d.kv("par2 version", build.Par2Version, false)
	d.kv("ltfs version", build.LTFSVersion, false)
	d.kv("Slice size", fmt.Sprintf("%s bytes (%s)", groupDigits(build.SliceSize), humanSize(build.SliceSize)), false)
	d.kv("PAR2 redundancy", build.PAR2Redundancy, false)
	// Tape generation leads: it is what a recoverer acts on; drive rows are provenance.
	d.kv("Tape generation (needed to read)", build.TapeGeneration, false)
	d.kv("Drive model (wrote the tapes)", build.DriveModel, false)
	d.kv("Drive generation", build.DriveGeneration, false)
	d.kv("Drive serial", build.DriveSerial, true)
	d.kv("Library model", build.LibraryModel, false)
}

func (d *doc) identitySection(m Manifest) {
	d.section("Encryption key — age private identity")

	d.pdf.SetFont(fontBody, "I", 9)
	d.text(colMuted)
	d.pdf.MultiCell(contentW, 5,
		d.tr("Included deliberately, so this report alone is enough to decrypt the archives with no other key store. Anyone holding it can decrypt them — store and dispose of it accordingly."),
		"", "L", false)
	d.pdf.Ln(2)

	d.fill(colKeyBG)
	d.draw(colKeyBdr)
	d.pdf.SetLineWidth(0.4)
	d.text(colInk)
	d.pdf.SetFont(fontMono, "", 10)
	d.pdf.MultiCell(contentW, 7, d.tr(m.AgeIdentity), "1", "L", true)
}

// recoverySection renders the procedure, classifying lines by leading indent into
// title, prose, hanging-indented numbered steps, and monospace command lines.
func (d *doc) recoverySection(m Manifest) {
	d.section("Recovery procedure")

	const (
		stepNumW  = 7.0 // gutter holding the step number
		cmdIndent = 6.0 // extra indent for command lines under a step body
		lineH     = 5.0
	)

	titleDone := false

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
			titleDone = true

			d.pdf.SetFont(fontBody, "B", 11)
			d.text(colInk)
			d.pdf.SetX(marginX)
			d.pdf.MultiCell(contentW, 6, d.tr(content), "", "L", false)
			d.pdf.Ln(0.5)

		case indent == 0 && number != "":
			// Keep the whole step block together so a page break never separates
			// an instruction from its command.
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
			d.pdf.SetFont(fontMono, "", 8.5)
			d.text(colInk)
			d.pdf.SetX(marginX + stepNumW + cmdIndent)
			d.pdf.MultiCell(contentW-stepNumW-cmdIndent, lineH, d.tr(content), "", "L", false)
			d.pdf.SetX(marginX)

		case indent >= 1:
			d.pdf.SetFont(fontBody, "", 10)
			d.text(colInk)
			d.pdf.SetX(marginX + stepNumW)
			d.pdf.MultiCell(contentW-stepNumW, lineH, d.tr(content), "", "L", false)
			d.pdf.SetX(marginX)

		default:
			d.pdf.SetFont(fontBody, "", 10)
			d.text(colInk)
			d.pdf.SetX(marginX)
			d.pdf.MultiCell(contentW, lineH, d.tr(content), "", "L", false)
		}
	}
}

// splitStep splits "N.  text" into "N." and text; number is empty for non-steps.
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

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// normalizeRecovery joins the source's hard-wrapped (~80 col) prose paragraphs
// into single lines so they wrap to the page cleanly. Blank lines and command
// lines (indent >= 8) pass through verbatim.
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
			flush()

			out = append(out, raw)
		case para.Len() > 0 && (indent != 0 || number == ""):
			para.WriteString(" ")
			para.WriteString(content)
		default:
			flush()

			paraIndent = indent

			para.WriteString(content)
		}
	}

	flush()

	return out
}

// wrappedHeight measures text wrapped to width (at least one line). It changes the active font.
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

// stepBlockHeight measures a step plus its indented command/continuation lines.
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

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}

	return strings.Join(items, ", ")
}

func compressionLabel(compressed bool) string {
	if compressed {
		return "zstd (decompress with zstd -d before untarring)"
	}

	return "none (the decrypted stream is already a tar)"
}

// groupDigits formats n with thousands separators, e.g. "1,073,741,824".
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

// humanSize renders n in binary units, e.g. "256.00 MiB".
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
