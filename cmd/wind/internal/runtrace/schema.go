package runtrace

import (
	"strconv"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

// RunTraceRecord adds CLI recording metadata to the application wire payload.
// Command results belong to the CLI; shared observation shapes belong to wire.
type RunTraceRecord struct {
	SchemaVersion int                 `json:"schema_version"`
	Sequence      string              `json:"sequence"`
	Time          string              `json:"time"`
	ElapsedMS     string              `json:"elapsed_ms"`
	Level         string              `json:"level"`
	Event         string              `json:"event"`
	Command       string              `json:"command"`
	RuntimeRunID  string              `json:"runtime_run_id"`
	Correlation   *wire.CorrelationV1 `json:"correlation,omitempty"`
	Payload       wire.Payload        `json:"payload"`
}

func baseRecord(
	runID runIdentity,
	metadata entryMetadata,
	command clievent.Command,
	level appobs.Level,
	event string,
) (RunTraceRecord, error) {
	commandName, commandOK := command.Name()
	levelName, levelOK := runTraceLevelName(level)
	if !runID.valid() || !commandOK || !levelOK || event == "" ||
		metadata.sequence == 0 || metadata.elapsedMS < 0 {
		return RunTraceRecord{}, ErrInvalidConfig
	}
	return RunTraceRecord{
		SchemaVersion: SchemaVersion,
		Sequence:      strconv.FormatUint(metadata.sequence, 10),
		Time:          metadata.time.UTC().Format(time.RFC3339Nano),
		ElapsedMS:     strconv.FormatInt(metadata.elapsedMS, 10),
		Level:         levelName,
		Event:         event,
		Command:       commandName,
		RuntimeRunID:  runID.encoded(),
		Payload:       wire.EmptyPayload{},
	}, nil
}

func summaryRecord(
	runID runIdentity,
	command clievent.Command,
	metadata entryMetadata,
	status Status,
) RunTraceRecord {
	level := appobs.LevelInfo
	if !status.Complete {
		level = appobs.LevelWarning
	}
	record, _ := baseRecord(runID, metadata, command, level, "trace_summary")
	record.Payload = traceSummaryPayload{
		ContractRejected:         decimal(status.ContractRejected),
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

func runTraceLevelName(level appobs.Level) (string, bool) {
	switch level {
	case appobs.LevelDebug:
		return "debug", true
	case appobs.LevelInfo:
		return "info", true
	case appobs.LevelWarning:
		return "warn", true
	case appobs.LevelError:
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
