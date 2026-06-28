package tape

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
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

// readTapeAlert runs "sg_logs --page=0x2e --json <sgDevice>" and parses the
// output. The JSON form is used rather than the decoded text because it is a
// stable, structured contract (it carries a json_format_version) and exposes
// the flag number, name, and value directly — the decoded text format omits
// the flag number for named flags and varies across sg3-utils releases.
func (r *LogPageReader) readTapeAlert(ctx context.Context) (TapeAlertResult, error) {
	out, err := exec.CommandContext(ctx, "sg_logs", "--page=0x2e", "--json", r.sgDevice).Output()
	if err != nil {
		return TapeAlertResult{}, fmt.Errorf("sg_logs --page=0x2e --json %s: %w", r.sgDevice, err)
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

// tapeAlertJSON models the subset of "sg_logs --page=0x2e --json" output we
// consume: each parameter carries its code (flag number), decoded meaning, and
// the flag value.
type tapeAlertJSON struct {
	Page struct {
		Params []struct {
			ParameterCode struct {
				Number  int    `json:"i"`
				Meaning string `json:"meaning"`
			} `json:"parameter_code"`
			Flag int `json:"flag"`
		} `json:"tapealert_log_parameters"`
	} `json:"tapealert_log_page"`
}

// parseTapeAlert parses the JSON output of "sg_logs --page=0x2e --json".
func parseTapeAlert(output string) (TapeAlertResult, error) {
	var doc tapeAlertJSON
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		return TapeAlertResult{}, fmt.Errorf("parse sg_logs TapeAlert JSON: %w", err)
	}

	var result TapeAlertResult

	for _, param := range doc.Page.Params {
		result.Flags = append(result.Flags, TapeAlertFlag{
			Number:      param.ParameterCode.Number,
			Description: param.ParameterCode.Meaning,
			Set:         param.Flag != 0,
		})
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
