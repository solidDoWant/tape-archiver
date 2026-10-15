package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToolVersion(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, ToolVersion())
}

func TestToolVersionStampedRevision(t *testing.T) {
	// Not parallel: mutates the package-level `version` used by ToolVersion.
	original := version

	t.Cleanup(func() { version = original })

	version = "abc1234"

	assert.Equal(t, "abc1234", ToolVersion())
}

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

func TestUnknownVersionFallback(t *testing.T) {
	t.Parallel()

	assert.Equal(t, unknownVersion, externalVersion("nonexistent-tool"))
}
