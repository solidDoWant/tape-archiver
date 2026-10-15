package tape

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

const (
	// The first command after a power-on or reset draws a UNIT ATTENTION, which
	// clears once reported, so a retry succeeds.
	sgLogsAttempts   = 5
	sgLogsRetryDelay = 500 * time.Millisecond
)

// runSgLogs invokes sg_logs, retrying a transient UNIT ATTENTION.
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

// LogPageReader reads a drive's SCSI log pages via sg_logs on its sg node.
type LogPageReader struct {
	sgDevice string
}

// NewLogPageReader returns a LogPageReader for an sg node (e.g. /dev/sg1).
func NewLogPageReader(sgDevice string) *LogPageReader {
	return &LogPageReader{sgDevice: sgDevice}
}

// ReadLogPages reads TapeAlert flags (page 0x2e) and the back-hitch count
// (page 0x30 total_suspended_writes).
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

// readTapeAlert uses sg_logs --json: the decoded text omits flag numbers for
// named flags and varies across sg3-utils releases.
func (r *LogPageReader) readTapeAlert(ctx context.Context) (TapeAlertResult, error) {
	out, err := runSgLogs(ctx, "--page=0x2e", "--json", r.sgDevice)
	if err != nil {
		return TapeAlertResult{}, fmt.Errorf("sg_logs --page=0x2e --json %s: %w", r.sgDevice, err)
	}

	return parseTapeAlert(string(out))
}

// readRepositions reads the back-hitch count from page 0x30. Drives without the
// page (not LTO-5/6) report not measured rather than an error, so the
// observational read never fails on them; malformed JSON is still an error.
func (r *LogPageReader) readRepositions(ctx context.Context) (count int64, measured bool, err error) {
	out, err := runSgLogs(ctx, "--page=0x30", "--json", r.sgDevice)
	if err != nil {
		return 0, false, nil
	}

	return parseTapeUsage(string(out))
}

// tapeAlertJSON models the consumed subset of "sg_logs --page=0x2e --json".
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

// tapeUsageParamSuspendedWrites carries total_suspended_writes: each is a
// mid-stream reposition, i.e. the shoe-shining SPEC §14 measures.
const tapeUsageParamSuspendedWrites = 5

// tapeUsageJSON models the consumed subset of "sg_logs --page=0x30 --json".
type tapeUsageJSON struct {
	Page struct {
		Params []struct {
			ParameterCode        int   `json:"parameter_code"`
			TotalSuspendedWrites int64 `json:"total_suspended_writes"`
		} `json:"tape_usage_log_parameters"`
	} `json:"tape_usage_log_page"`
}

// parseTapeUsage extracts total_suspended_writes; measured is false when the
// parameter is absent, so it is never confused with a measured zero.
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
