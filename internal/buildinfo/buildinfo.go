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

// version is the build version, stamped at link time via
// -ldflags "-X github.com/solidDoWant/tape-archiver/internal/buildinfo.version=<v>".
// The Nix image builds (nix/worker.nix, nix/web.nix) set it from the flake: the
// release version for a released build (supplied as a build argument, not
// committed and not read from a git tag), or the git revision for a development
// build. It has to be injected because they build from a lib.fileset source copy
// with no .git directory, so the binary cannot recover any of it from its own
// embedded build info at run time. When empty (a plain `go build`/`go test`),
// ToolVersion falls back to vcsRevision, which reads the Go build-info VCS stamp
// present in a `go build` from a git checkout.
var version string

// ToolVersion returns the tape-archiver build version: the release version for a
// released image (e.g. "1.2.3"), or the git revision for a development build,
// either of which identifies the running binary. It comes from the link-time
// stamp (Nix image builds) or, failing that, the Go build-info VCS record (a
// plain `go build` in a git checkout). It returns unknownVersion only when
// neither is available (e.g. `go test`), so the report and web UI never show a
// blank field.
func ToolVersion() string {
	if version != "" {
		return version
	}

	if revision := vcsRevision(); revision != "" {
		return revision
	}

	return unknownVersion
}

// vcsRevision returns the short VCS revision recorded in the binary's Go build
// info (with a "-dirty" suffix for an uncommitted tree), or "" when no VCS stamp
// is present (e.g. `go test`, or a build outside a git checkout). It is the
// fallback source of the commit identity for a plain `go build`, where the Nix
// link-time stamp is absent.
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
