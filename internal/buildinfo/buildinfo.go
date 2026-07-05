// Package buildinfo exposes the tape-archiver build version and the pinned
// versions of the external tools it shells out to (age, par2, ltfs, zstd, tar,
// xorriso).
// These feed the run report's build-metadata section (SPEC §9), which a future
// recoverer uses to understand how the tapes were produced.
//
// The external-tool versions are captured at build time from the pinned binaries
// by a generator (`make generate-versions`, wired to `go generate`) and committed
// in versions_generated.go, so they cannot silently drift from the tools the data
// worker actually runs. The tape-archiver version itself comes from the Go build
// info embedded in the binary, so it needs no generation.
package buildinfo

import "runtime/debug"

//go:generate go run ./gen -output versions_generated.go

// Tool names for the generated version map. They are the keys the generator
// writes and the accessors below read.
const (
	toolAge     = "age"
	toolPar2    = "par2"
	toolLTFS    = "ltfs"
	toolZstd    = "zstd"
	toolTar     = "tar"
	toolXorriso = "xorriso"
)

// unknownVersion is returned when a version could not be determined (no embedded
// build info, or a tool absent from the generated map). It is a visible
// placeholder rather than an empty string so a missing version is obvious in the
// report instead of rendering as a blank field.
const unknownVersion = "unknown"

// ToolVersion returns the tape-archiver build version: the module version when
// built as a dependency, or the VCS revision (with a "-dirty" suffix for an
// uncommitted tree) when built from source. It returns unknownVersion when no
// build information is embedded (e.g. `go test` without VCS stamping).
func ToolVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownVersion
	}

	if version := info.Main.Version; version != "" && version != "(devel)" {
		return version
	}

	var revision, modified string

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	if revision == "" {
		return unknownVersion
	}

	if modified == "true" {
		return revision + "-dirty"
	}

	return revision
}

// AgeVersion, Par2Version, LTFSVersion, ZstdVersion, TarVersion, and
// XorrisoVersion return the captured version of each external tool, or
// unknownVersion when it is absent from the generated map. XorrisoVersion is the
// libburnia burn tool bundled in the data-worker image only (SPEC §10); it is not
// on the recovery disc, which only needs to read ISO 9660.
func AgeVersion() string     { return externalVersion(toolAge) }
func Par2Version() string    { return externalVersion(toolPar2) }
func LTFSVersion() string    { return externalVersion(toolLTFS) }
func ZstdVersion() string    { return externalVersion(toolZstd) }
func TarVersion() string     { return externalVersion(toolTar) }
func XorrisoVersion() string { return externalVersion(toolXorriso) }

// externalVersion looks up a tool's captured version, returning unknownVersion
// when it is missing or empty.
func externalVersion(tool string) string {
	if version, ok := externalToolVersions[tool]; ok && version != "" {
		return version
	}

	return unknownVersion
}
