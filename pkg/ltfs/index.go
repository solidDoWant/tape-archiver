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

// indexSchemaSuffix is the extension of index files written by `-o capture_index`.
const indexSchemaSuffix = ".schema"

// canonicalIndexRe matches the canonical "`uuid`.schema" capture, which is the
// latest index; LTFS may also leave generation-suffixed copies ("`uuid`-3.schema").
// A negative "-N.schema" test would misclassify an all-digit final UUID group.
var canonicalIndexRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\` +
		indexSchemaSuffix + `$`)

// ReadIndex returns the index XML LTFS captured to the work directory at
// unmount, identical to the on-tape index, with no extra tape movement
// (SPEC.md §10, §14). It must be called after Unmount.
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

// indexCandidate is a work-directory file considered as the captured index.
type indexCandidate struct {
	name    string
	modTime time.Time
}

// errNoIndex means no index was captured, e.g. ReadIndex ran before Unmount.
var errNoIndex = fmt.Errorf("no captured index (.schema) file found; ReadIndex must be called after Unmount")

// errStaleIndex means every captured index predates this mount, so this cycle's
// capture silently failed. Returning a prior format's index would ship the
// wrong recovery map on the ISO (SPEC.md §10, §14).
var errStaleIndex = fmt.Errorf("captured index predates this mount; index capture did not occur this cycle")

// pickIndexFile chooses the captured index: candidates older than notBefore are
// stale, the canonical name is preferred, then the newest (name breaks ties).
func pickIndexFile(candidates []indexCandidate, notBefore time.Time) (string, error) {
	var schemas, canonical []indexCandidate

	var sawStale bool

	for _, candidate := range candidates {
		if !strings.HasSuffix(candidate.name, indexSchemaSuffix) {
			continue
		}

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

	sort.Slice(chosen, func(i, j int) bool {
		if chosen[i].modTime.Equal(chosen[j].modTime) {
			return chosen[i].name > chosen[j].name
		}

		return chosen[i].modTime.After(chosen[j].modTime)
	})

	return chosen[0].name, nil
}

// validateIndex rejects truncated or unrelated files: data must parse with an
// <ltfsindex> root.
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
