package backup

import (
	"fmt"
	"path"
	"strings"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// This file derives the descriptive on-tape archive directory label (SPEC §6).
// Each archive lands under archives/NNN-<label>/, where NNN is the zero-padded
// source index — kept as the ordering key and the guaranteed-unique prefix — and
// <label> is a short, sanitized, human-readable name for the source so an operator
// browsing a mounted LTFS volume can tell the archives apart without the report.
//
// The label is never relied on for uniqueness: two sources may sanitize to the
// same label, and the NNN prefix keeps their directories distinct. The slice and
// PAR2 basenames within the directory (archive.NNN, archive*.par2) are unchanged,
// so the documented recovery globs still match.

const (
	// maxLabelLen bounds the sanitized label so the on-tape path stays short and
	// well within any filesystem component limit, even after the NNN- prefix.
	maxLabelLen = 40
	// fallbackLabel is used when a source yields no usable label characters (e.g.
	// an override or name that sanitizes to the empty string). The NNN prefix still
	// disambiguates, so a generic label is safe here.
	fallbackLabel = "archive"
)

// sourceLabel is the sanitized directory label for a config source (SPEC §6). An
// operator-supplied Label override wins when it sanitizes to something non-empty;
// otherwise the label is derived from the source's identity: a raw ZFS source uses
// its dataset's last path component (any "@snapshot" stripped), a named k8s source
// uses the resource name, and a label-selector k8s source uses the selector. It
// always returns a non-empty label, falling back to fallbackLabel.
func sourceLabel(source config.Source) string {
	if source.Label != nil {
		if label := sanitizeLabel(*source.Label); label != "" {
			return label
		}
	}

	var raw string

	switch {
	case source.ZFSPath != nil:
		dataset, _, _ := strings.Cut(source.ZFSPath.Name, "@")
		raw = path.Base(dataset)
	case source.K8s != nil && source.K8s.Name != "":
		raw = source.K8s.Name
	case source.K8s != nil:
		raw = source.K8s.LabelSelector
	}

	if label := sanitizeLabel(raw); label != "" {
		return label
	}

	return fallbackLabel
}

// sanitizeLabel reduces an arbitrary source name to a bounded, filesystem-safe
// directory label containing only [a-z0-9._-] (SPEC §6). It lowercases the input,
// maps "/", "@", ":", whitespace, and every other out-of-range rune to "-",
// collapses runs of "-", trims leading/trailing "-"/".", and bounds the length to
// maxLabelLen. It returns the empty string when nothing usable remains; callers
// apply their own fallback.
func sanitizeLabel(raw string) string {
	var builder strings.Builder

	var lastDash bool

	for _, runeValue := range strings.ToLower(raw) {
		switch {
		case (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= '0' && runeValue <= '9') ||
			runeValue == '.' || runeValue == '_' || runeValue == '-':
			// Collapse consecutive dashes as we go rather than in a second pass.
			if runeValue == '-' && lastDash {
				continue
			}

			builder.WriteRune(runeValue)
			lastDash = runeValue == '-'
		default:
			// "/", "@", ":", whitespace, and anything else outside the allowed set
			// all become a single dash.
			if lastDash {
				continue
			}

			builder.WriteRune('-')

			lastDash = true
		}
	}

	label := strings.Trim(builder.String(), "-.")

	if len(label) > maxLabelLen {
		label = strings.Trim(label[:maxLabelLen], "-.")
	}

	return label
}

// archiveDirName is the on-tape directory for an archive: archives/NNN-<label>,
// where NNN is the zero-padded source index and <label> its sanitized name
// (SPEC §6). A missing label degrades to the bare archives/NNN rather than
// producing a trailing dash, so the path is always valid.
func archiveDirName(sourceIndex int, label string) string {
	if label == "" {
		return fmt.Sprintf("archives/%03d", sourceIndex)
	}

	return fmt.Sprintf("archives/%03d-%s", sourceIndex, label)
}
