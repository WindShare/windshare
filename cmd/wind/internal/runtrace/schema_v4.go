package runtrace

import (
	"strconv"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

// RunTraceRecordV4 is the stable NDJSON envelope. Event-specific data is
// package-sealed so adding one event cannot silently expand every other shape.
type RunTraceRecordV4 struct {
	SchemaVersion int            `json:"schema_version"`
	Sequence      string         `json:"sequence"`
	Time          string         `json:"time"`
	ElapsedMS     string         `json:"elapsed_ms"`
	Level         string         `json:"level"`
	Event         string         `json:"event"`
	Command       string         `json:"command"`
	RuntimeRunID  string         `json:"runtime_run_id"`
	Correlation   *CorrelationV1 `json:"correlation,omitempty"`
	Payload       payloadV4      `json:"payload"`
}

type payloadV4 interface {
	runTracePayloadV4()
}

type emptyPayloadV4 struct{}

func (emptyPayloadV4) runTracePayloadV4() {}

func baseRecordV4(
	runID runIdentity,
	metadata entryMetadata,
	command clievent.Command,
	level clievent.Level,
	event string,
) (RunTraceRecordV4, error) {
	commandName, commandOK := command.Name()
	levelName, levelOK := runTraceLevelName(level)
	if !runID.valid() || !commandOK || !levelOK || event == "" ||
		metadata.sequence == 0 || metadata.elapsedMS < 0 {
		return RunTraceRecordV4{}, ErrInvalidConfig
	}
	return RunTraceRecordV4{
		SchemaVersion: SchemaVersion,
		Sequence:      strconv.FormatUint(metadata.sequence, 10),
		Time:          metadata.time.UTC().Format(time.RFC3339Nano),
		ElapsedMS:     strconv.FormatInt(metadata.elapsedMS, 10),
		Level:         levelName,
		Event:         event,
		Command:       commandName,
		RuntimeRunID:  runID.encoded(),
		Payload:       emptyPayloadV4{},
	}, nil
}

func summaryV4(
	runID runIdentity,
	command clievent.Command,
	metadata entryMetadata,
	status Status,
) RunTraceRecordV4 {
	level := clievent.LevelInfo
	if !status.Complete {
		level = clievent.LevelWarning
	}
	record, _ := baseRecordV4(runID, metadata, command, level, "trace_summary")
	record.Payload = traceSummaryPayloadV4{
		Incomplete:               !status.Complete,
		RejectionEvidenceDropped: decimal(status.RejectionEvidenceDropped),
		LifecycleDropped:         decimal(status.LifecycleDropped),
		ProgressDropped:          decimal(status.ProgressDropped),
		EventsWritten:            decimal(status.EventsWritten),
		WriterFailed:             status.WriterFailed,
		FlushFailed:              status.FlushFailed,
		SchemaLimited:            status.SchemaLimited,
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
