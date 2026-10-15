package backup_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/checksum"
)

// These tests run docs/recovery-procedure.md's commands verbatim (placeholders
// substituted, bin/ tools taken from PATH), so the documented steps cannot drift
// from what actually works.

const (
	docBarcode    = "TAPE01L6"
	docArchiveDir = "000-test"
)

// TestRecoveryProcedure_RepairThenReassemble runs every documented PAR2 repair
// command on an archive with one damaged slice, then the documented reassembly.
func TestRecoveryProcedure_RepairThenReassemble(t *testing.T) {
	requireRecoveryTools(t)

	repairs := map[string]string{"step 4": docCodeBlock(t, "bin/par2 repair")}
	for i, command := range docFailureTableRepairs(t) {
		repairs[fmt.Sprintf("failure table %d", i)] = command
	}

	for name, command := range repairs {
		t.Run(name, func(t *testing.T) {
			scratch := t.TempDir()
			identityPath, recipient := generateAgeKeypair(t)
			slices, want := buildArchive(t, docArchiveDirPath(t, scratch), recipient)

			corruptFile(t, slices[len(slices)/2], 128, 200)

			got := recoverArchive(t, scratch, identityPath, command)
			assert.Equal(t, want, got, "the repaired, reassembled archive must recover the original bytes")
		})
	}
}

// TestRecoveryProcedure_CrossCheckAfterRepair runs the documented step-4 repair
// and then its SHA-256 cross-check on an undamaged archive.
func TestRecoveryProcedure_CrossCheckAfterRepair(t *testing.T) {
	requireRecoveryTools(t)

	scratch := t.TempDir()
	_, recipient := generateAgeKeypair(t)
	archiveDir := docArchiveDirPath(t, scratch)
	buildArchive(t, archiveDir, recipient)

	disc := t.TempDir()
	writeDiscManifest(t, filepath.Join(disc, "manifest.sha256"), archiveDir)

	runDocSnippetIn(t, scratch, disc, docCodeBlock(t, "bin/par2 repair"))
	runDocSnippetIn(t, scratch, disc, docCodeBlock(t, "sha256sum -c -"))
}

// docArchiveDirPath creates and returns the staged archive directory the
// procedure calls /scratch/NNN-<label>.
func docArchiveDirPath(t *testing.T, scratch string) string {
	t.Helper()

	dir := filepath.Join(scratch, docArchiveDir)
	require.NoError(t, os.Mkdir(dir, 0o755))

	return dir
}

// writeDiscManifest writes a disc manifest.sha256 covering every file in
// archiveDir (slices and PAR2 files), as the run does.
func writeDiscManifest(t *testing.T, path, archiveDir string) {
	t.Helper()

	entries, err := os.ReadDir(archiveDir)
	require.NoError(t, err)

	lines := make([]string, 0, len(entries))

	for _, entry := range entries {
		digest, err := checksum.SHA256File(filepath.Join(archiveDir, entry.Name()))
		require.NoError(t, err)

		lines = append(lines, fmt.Sprintf("%s  %s/archives/%s/%s", digest, docBarcode, docArchiveDir, entry.Name()))
	}

	sort.Strings(lines)
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

func procedureDoc(t *testing.T) string {
	t.Helper()

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "recovery-procedure.md"))
	require.NoError(t, err)

	return string(doc)
}

// docCodeBlock returns the one fenced code block in the procedure containing
// marker, with its indentation removed.
func docCodeBlock(t *testing.T, marker string) string {
	t.Helper()

	var (
		blocks  []string
		current []string
		inBlock bool
	)

	for _, line := range strings.Split(procedureDoc(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}

			inBlock = !inBlock

			continue
		}

		if inBlock {
			current = append(current, trimmed)
		}
	}

	var matches []string

	for _, block := range blocks {
		if strings.Contains(block, marker) {
			matches = append(matches, block)
		}
	}

	require.Lenf(t, matches, 1, "exactly one code block in the procedure must contain %q", marker)

	return matches[0]
}

// docFailureTableRepairs returns every par2 repair command in the procedure's
// failure-scenarios table.
func docFailureTableRepairs(t *testing.T) []string {
	t.Helper()

	pattern := regexp.MustCompile("`(bin/par2 repair [^`]+)`")

	var commands []string

	for _, line := range strings.Split(procedureDoc(t), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}

		for _, match := range pattern.FindAllStringSubmatch(line, -1) {
			commands = append(commands, match[1])
		}
	}

	require.NotEmpty(t, commands, "the failure table must document a par2 repair command")

	return commands
}

// runDocSnippet runs a documented snippet with /scratch mapped to scratch.
func runDocSnippet(t *testing.T, scratch, snippet string) {
	t.Helper()

	runDocSnippetIn(t, scratch, t.TempDir(), snippet)
}

// runDocSnippetIn runs a documented snippet with /scratch mapped to scratch and
// /path/to/disc to disc.
func runDocSnippetIn(t *testing.T, scratch, disc, snippet string) {
	t.Helper()

	script := strings.NewReplacer(
		"bin/par2", "par2",
		"bin/age", "age",
		"/path/to/disc", disc,
		"/scratch", scratch,
		"NNN-<label>", docArchiveDir,
		"<barcode>", docBarcode,
	).Replace(snippet)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-ec", script)
	cmd.Dir = scratch
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "documented command failed:\n%s\noutput:\n%s", script, out)
}
