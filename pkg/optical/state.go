package optical

import (
	"context"
	"fmt"
	"strings"
)

// DiscState is the state of the medium loaded in a drive, as reported by
// xorriso's media report. It drives the overwrite decision made above this seam:
// only a Blank disc is ready to burn, and only a NonBlankRewritable disc can be
// reclaimed with Blank.
type DiscState int

const (
	// StateUnknown means the medium's state could not be determined from
	// xorriso's report (an unrecognized report, or no medium loaded).
	StateUnknown DiscState = iota
	// StateBlank is an empty medium ready to be written.
	StateBlank
	// StateAppendableWriteOnce is a write-once medium (e.g. an M-DISC DVD-R)
	// that has been written but not finalized, so more could be appended. It is
	// not blank and cannot be reclaimed.
	StateAppendableWriteOnce
	// StateNonBlankRewritable is a rewritable medium (or the stdio pseudo-disc)
	// that holds data but can be reclaimed with Blank and rewritten.
	StateNonBlankRewritable
	// StateFinalized is a closed medium that can no longer be written or
	// appended to.
	StateFinalized
)

// String renders a DiscState for logs and errors.
func (s DiscState) String() string {
	switch s {
	case StateBlank:
		return "blank"
	case StateAppendableWriteOnce:
		return "appendable-write-once"
	case StateNonBlankRewritable:
		return "non-blank-rewritable"
	case StateFinalized:
		return "finalized"
	default:
		return "unknown"
	}
}

// State returns the state of the medium loaded in this disc's drive. It reads
// xorriso's media report (`-toc`) and classifies it; see parseMediaReport for
// the classification rules.
func (d *Disc) State(ctx context.Context) (DiscState, error) {
	info, err := d.probe(ctx)
	if err != nil {
		return StateUnknown, err
	}

	return info.state, nil
}

// mediaInfo is the parsed result of xorriso's media report: the classified state
// plus whether the medium is rewritable. Blank consults rewritable to refuse a
// write-once medium; State exposes only the classified state.
type mediaInfo struct {
	state      DiscState
	rewritable bool
	rawCurrent string
	rawStatus  string
}

// probe runs xorriso's media report against this disc's drive and parses it.
func (d *Disc) probe(ctx context.Context) (mediaInfo, error) {
	out, err := runXorriso(ctx, "-indev", d.driveAddress(), "-toc")
	if err != nil {
		return mediaInfo{}, fmt.Errorf("optical: reading media state of %s: %w", d.device, err)
	}

	info, err := parseMediaReport(out)
	if err != nil {
		return mediaInfo{}, fmt.Errorf("optical: media state of %s: %w", d.device, err)
	}

	return info, nil
}

// rewritableMediaTokens are substrings of xorriso's "Media current:" line that
// mark a rewritable (reclaimable) medium. "overwriteable" covers the stdio
// pseudo-disc and overwriteable optical media (DVD+RW, DVD-RAM, formatted BD-RE);
// the sequential rewritable types name themselves. A medium whose current-media
// line matches none of these is treated as write-once (DVD-R/M-DISC, DVD+R,
// CD-R, BD-R) — the conservative default, since misclassifying write-once as
// rewritable is the dangerous direction (Blank would attempt to reclaim it).
var rewritableMediaTokens = []string{"overwriteable", "DVD+RW", "DVD-RW", "DVD-RAM", "BD-RE"}

// parseMediaReport classifies xorriso's media report into a mediaInfo. It reads
// two lines xorriso always emits for a loaded medium:
//
//	Media current: <type>[, overwriteable]
//	Media status : is blank | is written , is appendable | is written , is closed
//
// Classification:
//   - "is blank"            → StateBlank
//   - "is closed"           → StateFinalized
//   - otherwise rewritable  → StateNonBlankRewritable
//   - otherwise (write-once)→ StateAppendableWriteOnce
//
// Rewritability comes from the current-media line (rewritableMediaTokens), not
// from "is appendable": overwriteable media also report "is appendable" after a
// write, so the status line alone cannot distinguish rewritable from write-once.
func parseMediaReport(report string) (mediaInfo, error) {
	var info mediaInfo

	for _, line := range strings.Split(report, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Media current:"):
			info.rawCurrent = strings.TrimSpace(strings.TrimPrefix(trimmed, "Media current:"))
		case strings.HasPrefix(trimmed, "Media status :"):
			info.rawStatus = strings.TrimSpace(strings.TrimPrefix(trimmed, "Media status :"))
		}
	}

	if info.rawCurrent == "" || info.rawStatus == "" {
		return mediaInfo{}, fmt.Errorf("no medium reported (Media current/status absent); is a disc loaded?")
	}

	for _, token := range rewritableMediaTokens {
		if strings.Contains(info.rawCurrent, token) {
			info.rewritable = true

			break
		}
	}

	switch {
	case strings.Contains(info.rawStatus, "is blank"):
		info.state = StateBlank
	case strings.Contains(info.rawStatus, "is closed"):
		info.state = StateFinalized
	case info.rewritable:
		info.state = StateNonBlankRewritable
	default:
		info.state = StateAppendableWriteOnce
	}

	return info, nil
}
