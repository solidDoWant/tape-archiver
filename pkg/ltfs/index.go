package ltfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// indexSchemaSuffix is the extension LTFS gives the index files it writes to the
// work directory under `-o capture_index`.
const indexSchemaSuffix = ".schema"

// canonicalIndexRe matches a canonical captured-index name: exactly
// "<uuid>.schema", where <uuid> is an RFC-4122 8-4-4-4-12 hex UUID. LTFS names
// the current index after the volume UUID ("<uuid>.schema") and may also leave
// generation-suffixed copies ("<uuid>-3.schema"); the canonical unsuffixed file
// is the latest index, so it is preferred.
//
// The match is positive (require the canonical shape) rather than a negative
// "-<n>.schema" test: an all-digit final UUID group (e.g. ...-000000000000) is
// still hex and so still matches, whereas the old negative test misclassified
// such a canonical name as generation-suffixed and excluded it.
var canonicalIndexRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\` +
		indexSchemaSuffix + `$`)

// ReadIndex returns the LTFS index XML for the volume, read from the index that
// LTFS captured to the work directory at unmount (`-o capture_index`). It must
// be called after Unmount.
//
// This captures the index with no extra tape movement: the returned XML is the
// same index LTFS wrote to the tape's index partition at unmount, dumped to disk
// as a side effect of that write (SPEC.md §14, and the recovery use in §10).
func (m *Mount) ReadIndex(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(m.workDir)
	if err != nil {
		return nil, fmt.Errorf("read LTFS work directory %s: %w", m.workDir, err)
	}

	candidates := make([]indexCandidate, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", filepath.Join(m.workDir, entry.Name()), err)
		}

		candidates = append(candidates, indexCandidate{name: entry.Name(), modTime: info.ModTime()})
	}

	name, err := pickIndexFile(candidates, m.mountStart)
	if err != nil {
		return nil, fmt.Errorf("locate captured LTFS index in %s: %w", m.workDir, err)
	}

	data, err := os.ReadFile(filepath.Join(m.workDir, name))
	if err != nil {
		return nil, fmt.Errorf("read captured LTFS index %s: %w", name, err)
	}

	if err := validateIndex(data); err != nil {
		return nil, fmt.Errorf("captured LTFS index %s is invalid: %w", name, err)
	}

	return data, nil
}

// indexCandidate is a file in the work directory considered as the captured
// index, paired with its modification time for tie-breaking.
type indexCandidate struct {
	name    string
	modTime time.Time
}

// errNoIndex is returned when the work directory holds no captured index — e.g.
// ReadIndex was called before Unmount, so LTFS never wrote one.
var errNoIndex = fmt.Errorf("no captured index (.schema) file found; ReadIndex must be called after Unmount")

// errStaleIndex is returned when the work directory holds captured index files
// but every one predates this mount cycle — i.e. they are leftovers from a prior
// format of the same barcode and this cycle's `-o capture_index` dump never
// appeared (a silent LTFS index-capture failure). Returning the stale index
// would ship a prior format's byte-level map on the recovery ISO (SPEC.md §10,
// §14), so the run must fail instead.
var errStaleIndex = fmt.Errorf("captured index predates this mount; index capture did not occur this cycle")

// pickIndexFile chooses the captured index among the work directory's files: any
// candidate older than notBefore (the mount's start time) is a leftover from a
// prior format and is rejected; among the rest the canonical unsuffixed
// "<uuid>.schema" is preferred over generation-suffixed copies, and the newest
// is chosen if several remain (name as a stable tie-breaker). It returns
// errNoIndex when no .schema file is present and errStaleIndex when .schema
// files exist but all predate notBefore.
func pickIndexFile(candidates []indexCandidate, notBefore time.Time) (string, error) {
	var schemas, canonical []indexCandidate

	var sawStale bool

	for _, candidate := range candidates {
		if !strings.HasSuffix(candidate.name, indexSchemaSuffix) {
			continue
		}

		// A capture written this cycle postdates the mount start; a leftover from
		// a prior format predates it. Reject stale leftovers so ReadIndex never
		// returns a previous format's index as this tape's recovery map.
		if candidate.modTime.Before(notBefore) {
			sawStale = true

			continue
		}

		schemas = append(schemas, candidate)

		if canonicalIndexRe.MatchString(candidate.name) {
			canonical = append(canonical, candidate)
		}
	}

	chosen := canonical
	if len(chosen) == 0 {
		chosen = schemas
	}

	if len(chosen) == 0 {
		if sawStale {
			return "", errStaleIndex
		}

		return "", errNoIndex
	}

	// Newest first; for equal mod times fall back to the name so the result is
	// deterministic.
	sort.Slice(chosen, func(i, j int) bool {
		if chosen[i].modTime.Equal(chosen[j].modTime) {
			return chosen[i].name > chosen[j].name
		}

		return chosen[i].modTime.After(chosen[j].modTime)
	})

	return chosen[0].name, nil
}

// validateIndex confirms data is a well-formed LTFS index XML document, so
// ReadIndex never returns a truncated or unrelated file as "the index". It
// checks the document parses and that its root element is <ltfsindex>.
func validateIndex(data []byte) error {
	var root struct {
		XMLName xml.Name
	}

	if err := xml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse index XML: %w", err)
	}

	if root.XMLName.Local != "ltfsindex" {
		return fmt.Errorf("unexpected root element %q, want ltfsindex", root.XMLName.Local)
	}

	return nil
}
