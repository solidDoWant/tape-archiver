package tape

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

const (
	// sgLogsAttempts / sgLogsRetryDelay bound the retry that absorbs a transient
	// UNIT ATTENTION. Just after the drive powers on or is reset, its first SCSI
	// command draws a UNIT ATTENTION check condition (sg_logs exits non-zero);
	// the condition is cleared once reported, so a retry succeeds. A persistent
	// failure still surfaces after the attempts are exhausted.
	sgLogsAttempts   = 5
	sgLogsRetryDelay = 500 * time.Millisecond
)

// runSgLogs invokes sg_logs, retrying to ride out a transient UNIT ATTENTION
// (see sgLogsAttempts). It returns the last attempt's output and error.
func runSgLogs(ctx context.Context, args ...string) ([]byte, error) {
	var (
		out []byte
		err error
	)

	for attempt := 0; attempt < sgLogsAttempts; attempt++ {
		out, err = exec.CommandContext(ctx, "sg_logs", args...).Output()
		if err == nil {
			return out, nil
		}

		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(sgLogsRetryDelay):
		}
	}

	return out, err
}

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

// ReadLogPages reads TapeAlert flags (page 0x2e) and the reposition/back-hitch
// count from the Tape usage log page (0x30, LTO-5/6) in one call. The reposition
// count is total_suspended_writes (parameter code 5) — the drive's write-
// suspension counter, each of which is a mid-stream stop-and-reposition.
func (r *LogPageReader) ReadLogPages(ctx context.Context) (LogPageResult, error) {
	taResult, err := r.readTapeAlert(ctx)
	if err != nil {
		return LogPageResult{}, err
	}

	repositions, measured, err := r.readRepositions(ctx)
	if err != nil {
		return LogPageResult{}, err
	}

	return LogPageResult{
		TapeAlert:           taResult,
		Repositions:         repositions,
		RepositionsMeasured: measured,
	}, nil
}

// readTapeAlert runs "sg_logs --page=0x2e --json <sgDevice>" and parses the
// output. The JSON form is used rather than the decoded text because it is a
// stable, structured contract (it carries a json_format_version) and exposes
// the flag number, name, and value directly — the decoded text format omits
// the flag number for named flags and varies across sg3-utils releases.
func (r *LogPageReader) readTapeAlert(ctx context.Context) (TapeAlertResult, error) {
	out, err := runSgLogs(ctx, "--page=0x2e", "--json", r.sgDevice)
	if err != nil {
		return TapeAlertResult{}, fmt.Errorf("sg_logs --page=0x2e --json %s: %w", r.sgDevice, err)
	}

	return parseTapeAlert(string(out))
}

// readRepositions runs "sg_logs --page=0x30 --json <sgDevice>" and extracts the
// reposition/back-hitch count (total_suspended_writes, parameter code 5). It
// returns the count, whether it was measured, and an error.
//
// A drive that does not support page 0x30 (not an LTO-5/6, so the page is not in
// its supported-pages list) fails the log-sense with ILLEGAL REQUEST; sg_logs
// exits non-zero. That is reported as (0, false, nil) — not measured — rather
// than an error, so an unsupported reposition counter never masks a good
// TapeAlert read and never fails the observational activity. A malformed JSON
// body from a page the drive did answer is a real fault and is returned as an
// error.
func (r *LogPageReader) readRepositions(ctx context.Context) (count int64, measured bool, err error) {
	out, err := runSgLogs(ctx, "--page=0x30", "--json", r.sgDevice)
	if err != nil {
		// Page 0x30 is LTO-5/6-specific; an unsupported drive rejects it. Treat
		// as not measured rather than failing the observational read.
		return 0, false, nil
	}

	return parseTapeUsage(string(out))
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

// tapeUsageParamSuspendedWrites is the Tape usage log page (0x30) parameter code
// carrying total_suspended_writes — the drive's write-suspension (back-hitch)
// count. A write suspension is a mid-stream stop-and-reposition, i.e. exactly the
// shoe-shining the anti-back-hitch measurement (SPEC §14) exists to detect.
const tapeUsageParamSuspendedWrites = 5

// tapeUsageJSON models the subset of "sg_logs --page=0x30 --json" output we
// consume: the Tape usage log page's parameter list. Each parameter carries its
// code and, for parameter code 5, total_suspended_writes.
type tapeUsageJSON struct {
	Page struct {
		Params []struct {
			ParameterCode        int   `json:"parameter_code"`
			TotalSuspendedWrites int64 `json:"total_suspended_writes"`
		} `json:"tape_usage_log_parameters"`
	} `json:"tape_usage_log_page"`
}

// parseTapeUsage extracts the reposition count (total_suspended_writes,
// parameter code 5) from "sg_logs --page=0x30 --json" output. measured is true
// only when the Tape usage page and that parameter are present; when the page or
// parameter is absent it returns (0, false, nil) so a not-measured counter is
// observable rather than indistinguishable from a measured zero. err is returned
// only when the JSON body is malformed.
func parseTapeUsage(output string) (count int64, measured bool, err error) {
	var doc tapeUsageJSON
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		return 0, false, fmt.Errorf("parse sg_logs Tape usage JSON: %w", err)
	}

	for _, param := range doc.Page.Params {
		if param.ParameterCode == tapeUsageParamSuspendedWrites {
			return param.TotalSuspendedWrites, true, nil
		}
	}

	return 0, false, nil
}
