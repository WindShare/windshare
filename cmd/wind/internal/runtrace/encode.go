package runtrace

import (
	"time"

	clievent "github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

var errInvalidSchemaEvent = wire.ErrUnsupportedEvent

type namedValue interface {
	Name() (string, bool)
}

func nameOf(value namedValue) (string, error) {
	name, ok := value.Name()
	if !ok {
		return "", errInvalidSchemaEvent
	}
	return name, nil
}

func (enc *cliEncoder) encodeCommandFailed(event clievent.CommandFailed) error {
	exitCode, ok := event.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	failure, err := wire.EncodeFailure(event.Failure())
	if err != nil {
		return err
	}
	enc.set("command_failed", nil, commandFailedPayload{
		ExitCode: exitCode, Failure: failure,
	})
	return nil
}

func (enc *cliEncoder) encodeTransferSettled(event clievent.TransferSettled) error {
	result := event.Result()
	status, err := nameOf(result.Status())
	if err != nil {
		return err
	}
	drift, err := nameOf(result.Drift())
	if err != nil {
		return err
	}
	exitCode, ok := result.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	payload := transferSettledPayload{
		SourceOmittedEntries: decimal(result.SourceOmittedEntries()),
		ResultStatus:         status,
		ExitCode:             exitCode,
		Drift:                drift,
		ResultElapsedMS:      signedDecimal(result.Elapsed().Milliseconds()),
		DestinationAdjusted:  result.DestinationAdjusted(),
		FileOutcomes:         wire.EncodeFileOutcomes(result.Files()),
		DirectoryFailures:    decimal(result.DirectoryFailures()),
		OmittedDiagnostics:   decimal(result.OmittedDiagnostics()),
		PublishedBytes:       decimal(result.PublishedBytes()),
		CountersExact:        result.CountersExact(),
	}
	if metrics, ok := event.DownloadConnectivity(); ok {
		summary := &downloadConnectivity{
			DownloadID: metrics.DownloadID, DirectBytes: decimal(metrics.DirectBytes),
			TURNBytes: decimal(metrics.TURNBytes), ApplicationRelayBytes: decimal(metrics.ApplicationRelayBytes),
			UnknownBytes: decimal(metrics.UnknownBytes), DirectFraction: metrics.DirectFraction,
			FallbackStallMS: signedDecimal(metrics.FallbackStall.Milliseconds()),
			Incomplete:      metrics.Incomplete, Final: metrics.Final,
		}
		if metrics.FirstDirectElapsed != nil {
			value := signedDecimal(metrics.FirstDirectElapsed.Milliseconds())
			summary.FirstDirectElapsedMS = &value
		}
		payload.DownloadConnectivity = summary
	}
	if failure, ok := result.Failure(); ok {
		projected, projectErr := wire.EncodeFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	enc.set("transfer_settled", nil, payload)
	return nil
}

func (enc *cliEncoder) encodeSharingStopped(event clievent.SharingStopped) error {
	result := event.Result()
	exitCode, ok := result.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	payload := sharingStoppedPayload{
		ExitCode:        exitCode,
		ResultElapsedMS: signedDecimal(result.Elapsed().Milliseconds()),
		StoppedCleanly:  result.StoppedCleanly(),
	}
	if failure, ok := result.Failure(); ok {
		projected, projectErr := wire.EncodeFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	enc.set("sharing_stopped", nil, payload)
	return nil
}

func (enc *cliEncoder) encodeTraceIncomplete(event clievent.TraceIncomplete) error {
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	enc.set("trace_incomplete", nil, traceIncompletePayload{
		Cause:            cause,
		ContractRejected: decimal(event.Loss().ContractRejections),
		LifecycleDropped: decimal(event.Loss().LifecycleDrops),
		ProgressDropped:  decimal(event.Loss().ProgressDrops),
	})
	return nil
}

func (enc *cliEncoder) encodePlatformSetupObserved(event clievent.PlatformSetupObserved) error {
	enc.set("platform_setup", nil, platformSetupPayload{State: event.State(), Reason: event.Reason()})
	return nil
}

type cliEncoder struct{ record *RunTraceRecord }

func (enc *cliEncoder) set(event string, correlation *wire.CorrelationV1, payload wire.Payload) {
	enc.record.Event, enc.record.Payload = event, payload
	if correlation != nil {
		enc.record.Correlation = correlation
	}
}
func encode(runID runIdentity, metadata entryMetadata, event clievent.Event) (RunTraceRecord, error) {
	if event == nil {
		return RunTraceRecord{}, clievent.ErrInvalidEvent
	}
	if err := event.Validate(); err != nil {
		return RunTraceRecord{}, err
	}
	record, err := baseRecord(runID, metadata, clievent.CommandOf(event), event.Level(), "pending")
	if err != nil {
		return RunTraceRecord{}, err
	}
	if fact, ok := event.(appobs.Event); ok {
		encoded, err := wire.Encode(fact)
		if err != nil {
			return RunTraceRecord{}, err
		}
		record.Event, record.Payload = encoded.Event, encoded.Payload
		if encoded.Correlation != nil {
			record.Correlation = encoded.Correlation
		}
		return record, nil
	}
	enc := &cliEncoder{record: &record}
	switch value := event.(type) {
	case clievent.Diagnostic:
		record.Event, record.Payload = value.Record.Name(), value.Record.Payload()
		if correlation := value.Record.Correlation(); correlation.Present() {
			record.Correlation = correlation
		}
		scope := value.Record.Scope()
		record.Scope = &scope
		if observedAt := value.Record.ObservedAt(); !observedAt.IsZero() {
			record.SourceTime = observedAt.UTC().Format(time.RFC3339Nano)
		}
	case clievent.CommandFailed:
		err = enc.encodeCommandFailed(value)
	case clievent.TransferSettled:
		err = enc.encodeTransferSettled(value)
	case clievent.SharingStopped:
		err = enc.encodeSharingStopped(value)
	case clievent.TraceIncomplete:
		err = enc.encodeTraceIncomplete(value)
	case clievent.PlatformSetupObserved:
		err = enc.encodePlatformSetupObserved(value)
	default:
		err = errInvalidSchemaEvent
	}
	if err != nil {
		return RunTraceRecord{}, err
	}
	return record, nil
}
