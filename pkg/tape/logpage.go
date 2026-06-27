package tape

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// LogPageReader reads SCSI log pages from a tape drive via sg_logs.
// It targets the SCSI generic device node (e.g. /dev/sg1).
type LogPageReader struct {
	sgDevice string
}

// NewLogPageReader returns a LogPageReader targeting the given SCSI generic
// device (e.g. /dev/sg1).
func NewLogPageReader(sgDevice string) *LogPageReader {
	return &LogPageReader{sgDevice: sgDevice}
}

// ReadLogPages reads TapeAlert flags (page 0x2e) and the reposition count from
// the sequential-access device log page (0x24) in one call.
func (r *LogPageReader) ReadLogPages(ctx context.Context) (LogPageResult, error) {
	taResult, err := r.readTapeAlert(ctx)
	if err != nil {
		return LogPageResult{}, err
	}

	repositions, err := r.readRepositions(ctx)
	if err != nil {
		return LogPageResult{}, err
	}

	return LogPageResult{
		TapeAlert:   taResult,
		Repositions: repositions,
	}, nil
}

// readTapeAlert runs "sg_logs --page=0x2e <sgDevice>" and parses the output.
func (r *LogPageReader) readTapeAlert(ctx context.Context) (TapeAlertResult, error) {
	out, err := exec.CommandContext(ctx, "sg_logs", "--page=0x2e", r.sgDevice).Output()
	if err != nil {
		return TapeAlertResult{}, fmt.Errorf("sg_logs --page=0x2e %s: %w", r.sgDevice, err)
	}

	return parseTapeAlert(string(out))
}

// readRepositions runs "sg_logs --page=0x24 <sgDevice>" and extracts the
// reposition/back-hitch count. Returns 0 when the page is not supported.
func (r *LogPageReader) readRepositions(ctx context.Context) (int64, error) {
	out, err := exec.CommandContext(ctx, "sg_logs", "--page=0x24", r.sgDevice).Output()
	if err != nil {
		// Some drives do not support this page; treat as zero rather than failing.
		return 0, nil
	}

	return parseRepositions(string(out)), nil
}

// tapeAlertFlagRe matches lines like:
//
//	TapeAlert flag 01h [Read warning]:
var tapeAlertFlagRe = regexp.MustCompile(`TapeAlert flag ([0-9a-fA-F]+)h \[([^\]]+)\]:`)

// tapeAlertValueRe matches the value line following a flag header:
//
//	[0x0]   or   [0x1]
var tapeAlertValueRe = regexp.MustCompile(`\[0x([01])\]`)

// parseTapeAlert parses the output of "sg_logs --page=0x2e".
//
// Example output:
//
//	TapeAlert log page (smc-3) [0x2e]:
//	  TapeAlert flag 01h [Read warning]:
//	    [0x0]
//	  TapeAlert flag 02h [Write warning]:
//	    [0x1]
func parseTapeAlert(output string) (TapeAlertResult, error) {
	var result TapeAlertResult

	lines := strings.Split(output, "\n")

	for i, line := range lines {
		m := tapeAlertFlagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		flagNum, err := strconv.ParseInt(m[1], 16, 32)
		if err != nil {
			return TapeAlertResult{}, fmt.Errorf("parse TapeAlert flag number %q: %w", m[1], err)
		}

		flag := TapeAlertFlag{
			Number:      int(flagNum),
			Description: m[2],
		}

		// Look ahead for the value line within the next few lines.
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if vm := tapeAlertValueRe.FindStringSubmatch(lines[j]); vm != nil {
				flag.Set = vm[1] == "1"
				break
			}
		}

		result.Flags = append(result.Flags, flag)
	}

	return result, nil
}

// repositionRe matches lines from the sequential-access log page that report
// tape repositions (back-hitches). The counter name varies by drive vendor.
var repositionRe = regexp.MustCompile(`(?i)(?:repositions?|back[- ]?hitch(?:es)?)[^=\n]*=\s*(\d+)`)

// parseRepositions extracts the reposition count from "sg_logs --page=0x24"
// output. Returns 0 when no matching field is found.
func parseRepositions(output string) int64 {
	m := repositionRe.FindStringSubmatch(output)
	if m == nil {
		return 0
	}

	n, _ := strconv.ParseInt(m[1], 10, 64)

	return n
}
