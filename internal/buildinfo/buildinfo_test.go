package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToolVersion checks that the tape-archiver version is always a non-empty
// string. Under `go test` the binary carries no VCS stamp, so the exact value is
// environment-dependent; the contract this asserts is that it never renders as an
// empty field in the report.
func TestToolVersion(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, ToolVersion())
}

// TestToolVersionStampedRevision checks that a link-time -ldflags stamp of the
// revision (what the Nix image builds inject) is reported verbatim as the version,
// overriding the build-info VCS fallback.
func TestToolVersionStampedRevision(t *testing.T) {
	// Not parallel: mutates the package-level `version` used by ToolVersion.
	original := version

	t.Cleanup(func() { version = original })

	version = "abc1234"

	assert.Equal(t, "abc1234", ToolVersion())
}

// TestExternalVersions checks the generated map is wired to the accessors: each
// tool the report records resolves to the value captured in versions_generated.go
// (regenerated with `make generate-versions`), never a blank field.
func TestExternalVersions(t *testing.T) {
	t.Parallel()

	accessors := map[string]func() string{
		toolAge:     AgeVersion,
		toolPar2:    Par2Version,
		toolLTFS:    LTFSVersion,
		toolZstd:    ZstdVersion,
		toolTar:     TarVersion,
		toolXorriso: XorrisoVersion,
	}

	for tool, accessor := range accessors {
		assert.Equal(t, externalToolVersions[tool], accessor(),
			"accessor for %s must return its generated version", tool)
		assert.NotEmpty(t, accessor(), "version for %s must not be empty", tool)
	}
}

// TestUnknownVersionFallback checks a tool absent from the map degrades to the
// visible placeholder rather than an empty string.
func TestUnknownVersionFallback(t *testing.T) {
	t.Parallel()

	assert.Equal(t, unknownVersion, externalVersion("nonexistent-tool"))
}
