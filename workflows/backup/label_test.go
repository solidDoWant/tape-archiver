package backup

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

func strPtr(s string) *string { return &s }

func TestSanitizeLabel(t *testing.T) {
	t.Parallel()

	const allowed = "abcdefghijklmnopqrstuvwxyz0123456789._-"

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "already clean", raw: "photos", want: "photos"},
		{name: "lowercased", raw: "PostgreSQL-Data", want: "postgresql-data"},
		{name: "slash becomes dash", raw: "pool/dataset", want: "pool-dataset"},
		{name: "at and colon become dash", raw: "tank/vm:disk@snap", want: "tank-vm-disk-snap"},
		{name: "whitespace becomes dash", raw: "my archive name", want: "my-archive-name"},
		{name: "label selector", raw: "app=myapp", want: "app-myapp"},
		{name: "collapses repeated separators", raw: "a///b   c", want: "a-b-c"},
		{name: "trims leading and trailing separators", raw: "--.name.--", want: "name"},
		{name: "keeps dots and underscores", raw: "ns__weird.name", want: "ns__weird.name"},
		{name: "empty stays empty", raw: "", want: ""},
		{name: "only separators sanitizes to empty", raw: "  @@// ", want: ""},
		{
			name: "truncated to the length bound without trailing dash",
			raw:  "a-really-long-volumesnapshot-name-that-exceeds-forty-characters",
			want: "a-really-long-volumesnapshot-name-that-e",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := sanitizeLabel(test.raw)
			assert.Equal(t, test.want, got)

			assert.LessOrEqual(t, len(got), maxLabelLen, "label must be bounded in length")

			for _, runeValue := range got {
				assert.True(t, strings.ContainsRune(allowed, runeValue),
					"sanitized label %q contains disallowed rune %q", got, runeValue)
			}
		})
	}
}

func TestSourceLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source config.Source
		want   string
	}{
		{
			name:   "raw zfs uses dataset last component, snapshot stripped",
			source: config.Source{ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/archive@daily-20260706"}},
			want:   "archive",
		},
		{
			name:   "raw zfs dataset without snapshot",
			source: config.Source{ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/media/photos"}},
			want:   "photos",
		},
		{
			name:   "k8s named resource uses the name",
			source: config.Source{K8s: &config.K8sRef{Namespace: "plex", Name: "plex-group-snap"}},
			want:   "plex-group-snap",
		},
		{
			name:   "k8s label selector uses the selector",
			source: config.Source{K8s: &config.K8sRef{Namespace: "default", LabelSelector: "app=myapp"}},
			want:   "app-myapp",
		},
		{
			name:   "override wins over derived",
			source: config.Source{Label: strPtr("Cold Storage"), ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/archive"}},
			want:   "cold-storage",
		},
		{
			name:   "override sanitizing to empty falls back to derived",
			source: config.Source{Label: strPtr("///"), ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/photos"}},
			want:   "photos",
		},
		{
			name:   "literal override that equals the fallback is honored",
			source: config.Source{Label: strPtr("archive"), ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/photos"}},
			want:   "archive",
		},
		{
			name:   "derived that sanitizes to nothing falls back",
			source: config.Source{ZFSPath: &config.ZFSPathSource{Name: "@@@"}},
			want:   fallbackLabel,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, sourceLabel(test.source))
		})
	}
}

// TestArchiveDirNameSharedLabel proves the on-tape directory is unique per source
// even when two sources produce the identical label, because the zero-padded source
// index prefixes the directory.
func TestArchiveDirNameSharedLabel(t *testing.T) {
	t.Parallel()

	first := archiveDirName(0, "archive")
	second := archiveDirName(1, "archive")

	assert.Equal(t, "archives/000-archive", first)
	assert.Equal(t, "archives/001-archive", second)
	assert.NotEqual(t, first, second, "same label on different sources must not collide")
}

// TestArchiveDirNameCollisionAcrossSources exercises every way two sources can
// arrive at the same label — two identical overrides, two identical derived names,
// and an override that matches another source's derived name — and asserts each
// pair still maps to distinct directories.
func TestArchiveDirNameCollisionAcrossSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    config.Source
		b    config.Source
	}{
		{
			name: "identical overrides",
			a:    config.Source{Label: strPtr("shared"), ZFSPath: &config.ZFSPathSource{Name: "pool/one"}},
			b:    config.Source{Label: strPtr("shared"), ZFSPath: &config.ZFSPathSource{Name: "pool/two"}},
		},
		{
			name: "derived names sanitize identically",
			a:    config.Source{K8s: &config.K8sRef{Namespace: "a", Name: "plex-group-snap"}},
			b:    config.Source{K8s: &config.K8sRef{Namespace: "b", Name: "plex/group@snap"}},
		},
		{
			name: "override matches another source's derived name",
			a:    config.Source{Label: strPtr("photos"), ZFSPath: &config.ZFSPathSource{Name: "pool/cold"}},
			b:    config.Source{ZFSPath: &config.ZFSPathSource{Name: "pool/photos"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			labelA := sourceLabel(test.a)
			labelB := sourceLabel(test.b)
			require.Equal(t, labelA, labelB, "precondition: this case must produce a shared label")

			dirA := archiveDirName(0, labelA)
			dirB := archiveDirName(1, labelB)
			assert.NotEqual(t, dirA, dirB, "shared label must still yield distinct directories")
		})
	}
}
