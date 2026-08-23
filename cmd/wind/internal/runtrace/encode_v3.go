package runtrace

import (
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

var errInvalidSchemaEvent = errors.New("event cannot be represented by trace schema v3")

type namedValue interface {
	Name() (string, bool)
}

type encodeVisitorV3 struct {
	record *RunTraceRecordV3
}

func encodeV3(
	runID runIdentity,
	metadata entryMetadata,
	event clievent.Event,
) (RunTraceRecordV3, error) {
	record, err := baseRecordV3(runID, metadata, event.Command(), event.Level(), "pending")
	if err != nil {
		return RunTraceRecordV3{}, err
	}
	visitor := &encodeVisitorV3{record: &record}
	if err := event.Accept(visitor); err != nil {
		return RunTraceRecordV3{}, err
	}
	if record.Event == "pending" || record.Payload == nil {
		return RunTraceRecordV3{}, errInvalidSchemaEvent
	}
	return record, nil
}

func nameOf(value namedValue) (string, error) {
	name, ok := value.Name()
	if !ok {
		return "", errInvalidSchemaEvent
	}
	return name, nil
}

func namedPointer(value namedValue) (*string, error) {
	name, err := nameOf(value)
	if err != nil {
		return nil, err
	}
	return &name, nil
}

func namesOf[T namedValue](values []T) ([]string, error) {
	names := make([]string, 0, len(values))
	for _, value := range values {
		name, err := nameOf(value)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func (visitor *encodeVisitorV3) set(event string, correlation *CorrelationV1, payload payloadV3) {
	visitor.record.Event = event
	visitor.record.Correlation = correlation
	visitor.record.Payload = payload
}

func projectEventCorrelation(
	session clievent.ProtocolSessionID,
	operation clievent.ProtocolOperationID,
	path clievent.PeerPathID,
	attempt clievent.PeerAttemptID,
	lane clievent.LaneIdentity,
	hasLane bool,
) (*CorrelationV1, error) {
	var projectedLane *LaneCorrelation
	if hasLane {
		if !lane.Valid() {
			return nil, errInvalidSchemaEvent
		}
		projectedLane = &LaneCorrelation{ID: lane.ID(), Epoch: lane.Epoch()}
	}
	correlation, err := ProjectCorrelationV1(CorrelationInput{
		ProtocolSessionID: session, ProtocolOperationID: operation,
		PeerPathID: path, PeerAttemptID: attempt, Lane: projectedLane,
	})
	if err != nil {
		return nil, errInvalidSchemaEvent
	}
	return correlation, nil
}

func projectSessionCorrelation(
	session clievent.ProtocolSessionID,
	lane clievent.LaneIdentity,
	hasLane bool,
) (*CorrelationV1, error) {
	return projectEventCorrelation(
		session, clievent.ProtocolOperationID{}, clievent.PeerPathID{},
		clievent.PeerAttemptID{}, lane, hasLane,
	)
}

func projectProtocolCorrelation(
	session clievent.ProtocolSessionID,
	operation clievent.ProtocolOperationID,
	lane clievent.LaneIdentity,
	hasLane bool,
) (*CorrelationV1, error) {
	return projectEventCorrelation(
		session, operation, clievent.PeerPathID{}, clievent.PeerAttemptID{}, lane, hasLane,
	)
}

func encodeTypedIdentity(raw []byte) string {
	return encodeCorrelationIdentity(raw)
}

func projectRelayAuthority(authority clievent.RelayAuthority) (relayAuthorityV3, error) {
	scheme, err := nameOf(authority.Scheme())
	if err != nil || !authority.Valid() {
		return relayAuthorityV3{}, errInvalidSchemaEvent
	}
	return relayAuthorityV3{Scheme: scheme, Host: authority.Host(), Port: authority.Port()}, nil
}

func projectFailure(failure clievent.Failure) (failureV3, error) {
	code, err := nameOf(failure.Code())
	if err != nil {
		return failureV3{}, err
	}
	messageKey, ok := failure.MessageKey()
	if !ok {
		return failureV3{}, errInvalidSchemaEvent
	}
	message, err := nameOf(messageKey)
	if err != nil {
		return failureV3{}, err
	}
	projected := failureV3{Code: code, MessageKey: message}
	if fault, ok := failure.Fault(); ok {
		domain, domainErr := nameOf(fault.Domain())
		scope, scopeErr := nameOf(fault.Scope())
		if domainErr != nil || scopeErr != nil {
			return failureV3{}, errInvalidSchemaEvent
		}
		projected.Fault = &faultV3{Domain: domain, Scope: scope, Code: fault.Code()}
	}
	if retryAfter, ok := failure.RetryAfterMillis(); ok {
		projected.RetryAfterMS = decimalPointer(retryAfter)
	}
	return projected, nil
}

func projectFileOutcomes(outcomes clievent.FileOutcomes) fileOutcomesV3 {
	return fileOutcomesV3{
		DownloadedFiles:      decimal(outcomes.DownloadedFiles),
		ResumedFiles:         decimal(outcomes.ResumedFiles),
		PausedFiles:          decimal(outcomes.PausedFiles),
		CollisionFiles:       decimal(outcomes.CollisionFiles),
		ItemBlockedFiles:     decimal(outcomes.ItemBlockedFiles),
		FailedFiles:          decimal(outcomes.FailedFiles),
		ModifiedTimeWarnings: decimal(outcomes.ModifiedTimeWarnings),
	}
}

func projectProgress(snapshot clievent.ProgressSnapshot) (progressPayloadV3, error) {
	discovery, err := nameOf(snapshot.Discovery())
	if err != nil || !snapshot.Valid() {
		return progressPayloadV3{}, errInvalidSchemaEvent
	}
	return progressPayloadV3{
		Discovery:          discovery,
		CountersExact:      snapshot.CountersExact(),
		DiscoveredFiles:    decimal(snapshot.DiscoveredFiles()),
		DiscoveredBytes:    decimal(snapshot.DiscoveredBytes()),
		PublishedFiles:     decimal(snapshot.PublishedFiles()),
		PublishedBytes:     decimal(snapshot.PublishedBytes()),
		VerifiedBytes:      decimal(snapshot.VerifiedBytes()),
		NewlyVerifiedBytes: decimal(snapshot.NewlyVerifiedBytes()),
		FileOutcomes:       projectFileOutcomes(snapshot.FileOutcomes()),
		CapacityWait: capacityWaitV3{
			ActiveWaiters:     decimal(uint64(snapshot.CapacityActiveWaiters())),
			AccumulatedWaitMS: signedDecimal(snapshot.CapacityAccumulatedWait().Milliseconds()),
			Attempts:          decimal(snapshot.CapacityWaitAttempts()),
		},
	}, nil
}

func (visitor *encodeVisitorV3) VisitReady(clievent.Ready) error {
	visitor.set("ready", nil, emptyPayloadV3{})
	return nil
}

func (visitor *encodeVisitorV3) VisitSharingSubjectSelected(event clievent.SharingSubjectSelected) error {
	subject := event.Subject()
	kind, err := nameOf(subject.Kind())
	if err != nil {
		return err
	}
	payload := sharingSubjectPayloadV3{
		SubjectKind: kind, SelectedItems: decimal(subject.SelectedItems()),
	}
	if subject.Kind() == clievent.SharingFile {
		payload.FileBytes = decimalPointer(subject.FileBytes())
	}
	visitor.set("sharing_subject_selected", nil, payload)
	return nil
}

func (visitor *encodeVisitorV3) VisitRelayConnected(event clievent.RelayConnected) error {
	authority, err := projectRelayAuthority(event.Authority())
	if err != nil {
		return err
	}
	visitor.set("relay_connected", nil, relayConnectedPayloadV3{RelayAuthority: authority})
	return nil
}

func (visitor *encodeVisitorV3) VisitRelayRecovering(event clievent.RelayRecovering) error {
	authority, err := projectRelayAuthority(event.Authority())
	if err != nil {
		return err
	}
	state, err := nameOf(event.State())
	if err != nil {
		return err
	}
	payload := relayRecoveringPayloadV3{
		RelayAuthority: authority, Attempt: event.Attempt(), State: state,
	}
	if failure, ok := event.Failure(); ok {
		projected, projectErr := projectFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	visitor.set("relay_recovering", nil, payload)
	return nil
}

func (visitor *encodeVisitorV3) VisitContentPathSelected(event clievent.ContentPathSelected) error {
	path, err := nameOf(event.Path())
	if err != nil {
		return err
	}
	visitor.set("content_path_selected", nil, contentPathSelectedPayloadV3{ContentPath: path})
	return nil
}

func (visitor *encodeVisitorV3) VisitFallback(event clievent.Fallback) error {
	from, err := nameOf(event.From())
	if err != nil {
		return err
	}
	to, err := nameOf(event.To())
	if err != nil {
		return err
	}
	failure, err := projectFailure(event.Failure())
	if err != nil {
		return err
	}
	visitor.set("fallback", nil, fallbackPayloadV3{
		FromTransport: from, ToTransport: to, Failure: failure,
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitTransferProgress(event clievent.TransferProgress) error {
	progress, err := projectProgress(event.Snapshot())
	if err != nil {
		return err
	}
	visitor.set("transfer_progress", nil, transferProgressPayloadV3{
		ReceiveOperationID: encodeTypedIdentity(event.ReceiveOperationID().Bytes()),
		TransferJobID:      encodeTypedIdentity(event.TransferJobID().Bytes()),
		Progress:           progress,
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitWarning(event clievent.Warning) error {
	failure, err := projectFailure(event.Failure())
	if err != nil {
		return err
	}
	visitor.set("warning", nil, warningPayloadV3{Failure: failure})
	return nil
}

func (visitor *encodeVisitorV3) VisitCommandFailed(event clievent.CommandFailed) error {
	exitCode, ok := event.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	failure, err := projectFailure(event.Failure())
	if err != nil {
		return err
	}
	visitor.set("command_failed", nil, commandFailedPayloadV3{
		ExitCode: exitCode, Failure: failure,
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitTransferSettled(event clievent.TransferSettled) error {
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
	payload := transferSettledPayloadV3{
		ResultStatus:        status,
		ExitCode:            exitCode,
		Drift:               drift,
		ResultElapsedMS:     signedDecimal(result.Elapsed().Milliseconds()),
		DestinationAdjusted: result.DestinationAdjusted(),
		FileOutcomes:        projectFileOutcomes(result.Files()),
		DirectoryFailures:   decimal(result.DirectoryFailures()),
		OmittedDiagnostics:  decimal(result.OmittedDiagnostics()),
		PublishedBytes:      decimal(result.PublishedBytes()),
		CountersExact:       result.CountersExact(),
	}
	if failure, ok := result.Failure(); ok {
		projected, projectErr := projectFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	visitor.set("transfer_settled", nil, payload)
	return nil
}

func (visitor *encodeVisitorV3) VisitSharingStopped(event clievent.SharingStopped) error {
	result := event.Result()
	exitCode, ok := result.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	payload := sharingStoppedPayloadV3{
		ExitCode:        exitCode,
		ResultElapsedMS: signedDecimal(result.Elapsed().Milliseconds()),
		StoppedCleanly:  result.StoppedCleanly(),
	}
	if failure, ok := result.Failure(); ok {
		projected, projectErr := projectFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	visitor.set("sharing_stopped", nil, payload)
	return nil
}

func (visitor *encodeVisitorV3) VisitTraceIncomplete(event clievent.TraceIncomplete) error {
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	visitor.set("trace_incomplete", nil, traceIncompletePayloadV3{
		Cause:            cause,
		LifecycleDropped: decimal(event.LifecycleDrops()),
		ProgressDropped:  decimal(event.ProgressDrops()),
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitLaneAdopted(event clievent.LaneAdopted) error {
	transport, err := nameOf(event.Transport())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(event.ProtocolSessionID(), event.Lane(), true)
	if err != nil {
		return err
	}
	visitor.set("lane_adopted", correlation, laneAdoptedPayloadV3{Transport: transport})
	return nil
}
