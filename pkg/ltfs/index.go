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

// genSuffixRe matches a generation-suffixed schema name (e.g.
// "<uuid>-3.schema"). LTFS names the current index "<uuid>.schema" and may also
// leave generation-suffixed copies; the unsuffixed file is the canonical latest
// index, so it is preferred.
var genSuffixRe = regexp.MustCompile(`-[0-9]+\` + indexSchemaSuffix + `$`)

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

	name, err := pickIndexFile(candidates)
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

// pickIndexFile chooses the captured index among the work directory's files: the
// canonical unsuffixed "<uuid>.schema" is preferred over generation-suffixed
// copies, and the newest is chosen if several remain (name as a stable
// tie-breaker). It returns errNoIndex when no .schema file is present.
func pickIndexFile(candidates []indexCandidate) (string, error) {
	var schemas, canonical []indexCandidate

	for _, candidate := range candidates {
		if !strings.HasSuffix(candidate.name, indexSchemaSuffix) {
			continue
		}

		schemas = append(schemas, candidate)

		if !genSuffixRe.MatchString(candidate.name) {
			canonical = append(canonical, candidate)
		}
	}

	chosen := canonical
	if len(chosen) == 0 {
		chosen = schemas
	}

	if len(chosen) == 0 {
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
