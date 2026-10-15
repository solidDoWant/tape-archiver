// Package buildinfo exposes the tape-archiver build version and the pinned
// external tool versions, for the report's build-metadata section so a future
// recoverer knows how the tapes were produced. Tool versions are captured from
// the pinned binaries at generate time so they cannot drift from what runs.
package buildinfo

import "runtime/debug"

//go:generate go run ./gen -output versions_generated.go

// Keys of the generated version map.
const (
	toolAge     = "age"
	toolPar2    = "par2"
	toolLTFS    = "ltfs"
	toolZstd    = "zstd"
	toolTar     = "tar"
	toolXorriso = "xorriso"
)

// unknownVersion is a visible placeholder so a missing version is obvious in the
// report rather than blank.
const unknownVersion = "unknown"

// version is stamped at link time (-X ...buildinfo.version=...) by the Nix image
// builds, whose fileset source copy has no .git for the Go VCS stamp to read.
var version string

// ToolVersion returns the tape-archiver release version or git revision, or
// unknownVersion when neither is available (e.g. `go test`).
func ToolVersion() string {
	if version != "" {
		return version
	}

	if revision := vcsRevision(); revision != "" {
		return revision
	}

	return unknownVersion
}

// vcsRevision returns the short Go build-info VCS revision (with "-dirty" for an
// uncommitted tree), or "" when absent.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
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
		return ""
	}

	// Match the length of the flake's self.shortRev stamp so the two build paths
	// render comparable revisions.
	const shortRevLen = 7
	if len(revision) > shortRevLen {
		revision = revision[:shortRevLen]
	}

	if modified == "true" {
		return revision + "-dirty"
	}

	return revision
}

// AgeVersion and the functions below return each external tool's captured
// version, or unknownVersion. xorriso is in the data-worker image only, not on
// the recovery disc.
func AgeVersion() string     { return externalVersion(toolAge) }
func Par2Version() string    { return externalVersion(toolPar2) }
func LTFSVersion() string    { return externalVersion(toolLTFS) }
func ZstdVersion() string    { return externalVersion(toolZstd) }
func TarVersion() string     { return externalVersion(toolTar) }
func XorrisoVersion() string { return externalVersion(toolXorriso) }

func externalVersion(tool string) string {
	if version, ok := externalToolVersions[tool]; ok && version != "" {
		return version
	}

	return unknownVersion
}
