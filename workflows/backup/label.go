package backup

import (
	"fmt"
	"path"
	"strings"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// Each archive lands under archives/NNN-<label>/ (SPEC §6): NNN is the
// zero-padded source index and the unique ordering key; <label> is a sanitized,
// human-readable source name and never relied on for uniqueness.

const (
	maxLabelLen   = 40
	fallbackLabel = "archive"
)

// sourceLabel is the sanitized directory label for a source (SPEC §6): a
// non-empty Label override, else the dataset's last path component, the k8s
// resource name, or the selector. It never returns "".
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

// sanitizeLabel reduces raw to a [a-z0-9._-] label of at most maxLabelLen,
// collapsing runs of "-" and trimming "-"/"."; it may return "".
func sanitizeLabel(raw string) string {
	var builder strings.Builder

	var lastDash bool

	for _, runeValue := range strings.ToLower(raw) {
		switch {
		case (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= '0' && runeValue <= '9') ||
			runeValue == '.' || runeValue == '_' || runeValue == '-':
			if runeValue == '-' && lastDash {
				continue
			}

			builder.WriteRune(runeValue)
			lastDash = runeValue == '-'
		default:
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

// archiveDirName is archives/NNN-<label> (SPEC §6), or archives/NNN without a
// label.
func archiveDirName(sourceIndex int, label string) string {
	if label == "" {
		return fmt.Sprintf("archives/%03d", sourceIndex)
	}

	return fmt.Sprintf("archives/%03d-%s", sourceIndex, label)
}
