package wire

import (
	appobs "github.com/windshare/windshare/engine/observation"
)

func nameOf(value namedValue) (string, error) {
	name, ok := value.Name()
	if !ok {
		return "", errInvalidSchemaEvent
	}
	return name, nil
}

func (enc *encoder) set(event string, correlation *CorrelationV1, payload Payload) {
	enc.record.Event = event
	enc.record.Correlation = correlation
	enc.record.Payload = payload
}

func projectEventCorrelation(
	session appobs.ProtocolSessionID,
	operation appobs.ProtocolOperationID,
	path appobs.PeerPathID,
	attempt appobs.PeerAttemptID,
	lane appobs.LaneIdentity,
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
	session appobs.ProtocolSessionID,
	lane appobs.LaneIdentity,
	hasLane bool,
) (*CorrelationV1, error) {
	return projectEventCorrelation(
		session, appobs.ProtocolOperationID{}, appobs.PeerPathID{},
		appobs.PeerAttemptID{}, lane, hasLane,
	)
}

func projectRelayOrigin(origin appobs.RelayOrigin) (RelayOrigin, error) {
	scheme, err := nameOf(origin.Scheme())
	if err != nil || !origin.Valid() {
		return RelayOrigin{}, errInvalidSchemaEvent
	}
	return RelayOrigin{Scheme: scheme, Host: origin.Host(), Port: origin.Port()}, nil
}

func EncodeFailure(failure appobs.Failure) (Failure, error) {
	code, err := nameOf(failure.Code())
	if err != nil {
		return Failure{}, err
	}
	messageKey, ok := failure.MessageKey()
	if !ok {
		return Failure{}, errInvalidSchemaEvent
	}
	message, err := nameOf(messageKey)
	if err != nil {
		return Failure{}, err
	}
	projected := Failure{Code: code, MessageKey: message}
	if fault, ok := failure.Fault(); ok {
		domain, domainErr := nameOf(fault.Domain())
		scope, scopeErr := nameOf(fault.Scope())
		if domainErr != nil || scopeErr != nil {
			return Failure{}, errInvalidSchemaEvent
		}
		projected.Fault = &Fault{Domain: domain, Scope: scope, Code: fault.Code()}
	}
	if retryAfter, ok := failure.RetryAfterMillis(); ok {
		projected.RetryAfterMS = decimalPointer(retryAfter)
	}
	if path, ok := failure.SourcePath(); ok {
		projected.SourcePath = path.Text()
	}
	return projected, nil
}

func EncodeFileOutcomes(outcomes appobs.FileOutcomes) FileOutcomes {
	return FileOutcomes{
		DownloadedFiles:          decimal(outcomes.DownloadedFiles),
		ResumedFiles:             decimal(outcomes.ResumedFiles),
		PreviouslyPublishedFiles: decimal(outcomes.PreviouslyPublishedFiles),
		PausedFiles:              decimal(outcomes.PausedFiles),
		CollisionFiles:           decimal(outcomes.CollisionFiles),
		ItemBlockedFiles:         decimal(outcomes.ItemBlockedFiles),
		FailedFiles:              decimal(outcomes.FailedFiles),
		ModifiedTimeWarnings:     decimal(outcomes.ModifiedTimeWarnings),
	}
}

func EncodeProgress(snapshot appobs.ProgressSnapshot) (ProgressPayload, error) {
	discovery, err := nameOf(snapshot.Discovery())
	if err != nil || !snapshot.Valid() {
		return ProgressPayload{}, errInvalidSchemaEvent
	}
	return ProgressPayload{
		SourceOmittedEntries:     decimal(snapshot.SourceOmittedEntries()),
		Discovery:                discovery,
		CountersExact:            snapshot.CountersExact(),
		DiscoveredFiles:          decimal(snapshot.DiscoveredFiles()),
		DiscoveredBytes:          decimal(snapshot.DiscoveredBytes()),
		PublishedFiles:           decimal(snapshot.PublishedFiles()),
		PublishedBytes:           decimal(snapshot.PublishedBytes()),
		VerifiedBytes:            decimal(snapshot.VerifiedBytes()),
		PreviouslyPublishedBytes: decimal(snapshot.PreviouslyPublishedBytes()),
		NewlyVerifiedBytes:       decimal(snapshot.NewlyVerifiedBytes()),
		RestartReusableBytes:     decimal(snapshot.RestartReusableBytes()),
		FileOutcomes:             EncodeFileOutcomes(snapshot.FileOutcomes()),
		CapacityWait: CapacityWait{
			ActiveWaiters:     decimal(uint64(snapshot.CapacityActiveWaiters())),
			AccumulatedWaitMS: signedDecimal(snapshot.CapacityAccumulatedWait().Milliseconds()),
			Attempts:          decimal(snapshot.CapacityWaitAttempts()),
		},
	}, nil
}

func (enc *encoder) encodeReady(appobs.Ready) error {
	enc.set("ready", nil, EmptyPayload{})
	return nil
}

func (enc *encoder) encodeSharingSubjectSelected(event appobs.SharingSubjectSelected) error {
	subject := event.Subject()
	kind, err := nameOf(subject.Kind())
	if err != nil {
		return err
	}
	payload := SharingSubjectPayload{
		SubjectKind: kind, SelectedItems: decimal(subject.SelectedItems()),
	}
	if subject.Kind() == appobs.SharingFile {
		payload.FileBytes = decimalPointer(subject.FileBytes())
	}
	enc.set("sharing_subject_selected", nil, payload)
	return nil
}

func (enc *encoder) encodeRelayConnected(event appobs.RelayConnected) error {
	origin, err := projectRelayOrigin(event.Origin())
	if err != nil {
		return err
	}
	enc.set("relay_connected", nil, RelayConnectedPayload{RelayOrigin: origin})
	return nil
}

func (enc *encoder) encodeRelayAvailability(event appobs.RelayAvailability) error {
	enc.set("relay_availability", nil, RelayAvailabilityPayload{event.Available(), event.Total(), event.EverReady(), event.Terminal()})
	return nil
}

func (enc *encoder) encodeRelayRecovering(event appobs.RelayRecovering) error {
	origin, err := projectRelayOrigin(event.Origin())
	if err != nil {
		return err
	}
	state, err := nameOf(event.State())
	if err != nil {
		return err
	}
	payload := RelayRecoveringPayload{
		RelayOrigin: origin, Attempt: event.Attempt(), State: state,
	}
	if failure, ok := event.Failure(); ok {
		projected, projectErr := EncodeFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	var correlation *CorrelationV1
	if details, ok := event.Details(); ok {
		payload.Details = &RelayRecoveryDetails{Generation: decimal(details.Generation), Slow: details.Slow, Resume: details.Resume, Terminal: details.Terminal, NextDelayMS: signedDecimal(details.NextDelay.Milliseconds())}
		if details.ShareInstance.Valid() {
			identity := EncodeIdentity(details.ShareInstance.Bytes())
			payload.Details.ShareInstance = &identity
		}
		if details.ProtocolSessionID.Valid() {
			var err error
			correlation, err = projectSessionCorrelation(details.ProtocolSessionID, appobs.LaneIdentity{}, false)
			if err != nil {
				return err
			}
		}
	}
	enc.set("relay_recovering", correlation, payload)
	return nil
}

func (enc *encoder) encodeContentPathSelected(event appobs.ContentPathSelected) error {
	path, err := nameOf(event.Path())
	if err != nil {
		return err
	}
	enc.set("content_path_selected", nil, ContentPathSelectedPayload{ContentPath: path})
	return nil
}

func (enc *encoder) encodeFallback(event appobs.Fallback) error {
	from, err := nameOf(event.From())
	if err != nil {
		return err
	}
	to, err := nameOf(event.To())
	if err != nil {
		return err
	}
	failure, err := EncodeFailure(event.Failure())
	if err != nil {
		return err
	}
	enc.set("fallback", nil, FallbackPayload{
		FromTransport: from, ToTransport: to, Failure: failure,
	})
	return nil
}

func (enc *encoder) encodeTransferProgress(event appobs.TransferProgress) error {
	progress, err := EncodeProgress(event.Snapshot())
	if err != nil {
		return err
	}
	enc.set("transfer_progress", nil, TransferProgressPayload{
		ReceiveOperationID: EncodeIdentity(event.ReceiveOperationID().Bytes()),
		TransferJobID:      EncodeIdentity(event.TransferJobID().Bytes()),
		Progress:           progress,
	})
	return nil
}

func (enc *encoder) encodeWarning(event appobs.Warning) error {
	failure, err := EncodeFailure(event.Failure())
	if err != nil {
		return err
	}
	enc.set("warning", nil, WarningPayload{Failure: failure})
	return nil
}

func (enc *encoder) encodeLaneAdopted(event appobs.LaneAdopted) error {
	transport, err := nameOf(event.Transport())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(event.ProtocolSessionID(), event.Lane(), true)
	if err != nil {
		return err
	}
	enc.set("lane_adopted", correlation, LaneAdoptedPayload{Transport: transport})
	return nil
}
