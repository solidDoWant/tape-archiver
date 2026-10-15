package optical

import (
	"context"
	"fmt"
	"strings"
)

// DiscState is the state of the loaded medium. Only StateBlank is ready to burn;
// only StateNonBlankRewritable can be reclaimed with Blank.
type DiscState int

const (
	StateUnknown DiscState = iota
	StateBlank
	// StateAppendableWriteOnce is written but not finalized; it cannot be reclaimed.
	StateAppendableWriteOnce
	StateNonBlankRewritable
	StateFinalized
)

// String renders a DiscState.
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

// State returns the state of the loaded medium, from xorriso's `-toc` report.
func (d *Disc) State(ctx context.Context) (DiscState, error) {
	info, err := d.probe(ctx)
	if err != nil {
		return StateUnknown, err
	}

	return info.state, nil
}

type mediaInfo struct {
	state      DiscState
	rewritable bool
	rawCurrent string
	rawStatus  string
}

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

// rewritableMediaTokens mark a rewritable medium in xorriso's "Media current:"
// line. Anything else is treated as write-once: the conservative default, since
// misclassifying write-once as rewritable would let Blank try to reclaim it.
var rewritableMediaTokens = []string{"overwriteable", "DVD+RW", "DVD-RW", "DVD-RAM", "BD-RE"}

// parseMediaReport classifies xorriso's "Media current:" and "Media status :"
// lines. Rewritability comes from the current-media line, not "is appendable":
// overwriteable media also report "is appendable" after a write.
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
