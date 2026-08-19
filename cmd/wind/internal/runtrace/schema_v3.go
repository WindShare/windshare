package runtrace

import (
	"strconv"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

// RunTraceRecordV3 is the stable NDJSON envelope. Event-specific data is
// package-sealed so adding one event cannot silently expand every other shape.
type RunTraceRecordV3 struct {
	SchemaVersion int            `json:"schema_version"`
	Sequence      string         `json:"sequence"`
	Time          string         `json:"time"`
	ElapsedMS     string         `json:"elapsed_ms"`
	Level         string         `json:"level"`
	Event         string         `json:"event"`
	Command       string         `json:"command"`
	RuntimeRunID  string         `json:"runtime_run_id"`
	Correlation   *CorrelationV1 `json:"correlation,omitempty"`
	Payload       payloadV3      `json:"payload"`
}

type payloadV3 interface {
	runTracePayloadV3()
}

type emptyPayloadV3 struct{}

func (emptyPayloadV3) runTracePayloadV3() {}

func baseRecordV3(
	runID runIdentity,
	metadata entryMetadata,
	command clievent.Command,
	level clievent.Level,
	event string,
) (RunTraceRecordV3, error) {
	commandName, commandOK := command.Name()
	levelName, levelOK := runTraceLevelName(level)
	if !runID.valid() || !commandOK || !levelOK || event == "" ||
		metadata.sequence == 0 || metadata.elapsedMS < 0 {
		return RunTraceRecordV3{}, ErrInvalidConfig
	}
	return RunTraceRecordV3{
		SchemaVersion: SchemaVersion,
		Sequence:      strconv.FormatUint(metadata.sequence, 10),
		Time:          metadata.time.UTC().Format(time.RFC3339Nano),
		ElapsedMS:     strconv.FormatInt(metadata.elapsedMS, 10),
		Level:         levelName,
		Event:         event,
		Command:       commandName,
		RuntimeRunID:  runID.encoded(),
		Payload:       emptyPayloadV3{},
	}, nil
}

func summaryV3(
	runID runIdentity,
	command clievent.Command,
	metadata entryMetadata,
	status Status,
) RunTraceRecordV3 {
	level := clievent.LevelInfo
	if !status.Complete {
		level = clievent.LevelWarning
	}
	record, _ := baseRecordV3(runID, metadata, command, level, "trace_summary")
	record.Payload = traceSummaryPayloadV3{
		Incomplete:       !status.Complete,
		LifecycleDropped: decimal(status.LifecycleDropped),
		ProgressDropped:  decimal(status.ProgressDropped),
		EventsWritten:    decimal(status.EventsWritten),
		WriterFailed:     status.WriterFailed,
		FlushFailed:      status.FlushFailed,
		SchemaLimited:    status.SchemaLimited,
	}
	return record
}

func runTraceLevelName(level clievent.Level) (string, bool) {
	switch level {
	case clievent.LevelDebug:
		return "debug", true
	case clievent.LevelInfo:
		return "info", true
	case clievent.LevelWarning:
		return "warn", true
	case clievent.LevelError:
		return "error", true
	default:
		return "", false
	}
}

func decimal(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func signedDecimal(value int64) string {
	return strconv.FormatInt(value, 10)
}

func decimalPointer(value uint64) *string {
	encoded := decimal(value)
	return &encoded
}
