package wire

import (
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
)

func (enc *encoder) encodeSenderTerminalSendObserved(
	event appobs.SenderTerminalSendObserved,
) error {
	transport, err := nameOf(event.TransportDisposition())
	if err != nil {
		return err
	}
	outcome, err := nameOf(event.Outcome())
	if err != nil {
		return err
	}
	decision, err := nameOf(event.Decision())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(
		event.ProtocolSessionID(), event.Lane(), true,
	)
	if err != nil {
		return err
	}
	enc.set("sender_terminal_send_observed", correlation, SenderTerminalSendPayload{
		Settled:              event.Settled(),
		TransportDisposition: transport,
		Outcome:              outcome,
		Decision:             decision,
	})
	return nil
}

func (enc *encoder) encodeSenderSessionTerminated(
	event appobs.SenderSessionTerminated,
) error {
	trigger, err := nameOf(event.Trigger())
	if err != nil {
		return err
	}
	provenance, err := nameOf(event.Provenance())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(
		event.ProtocolSessionID(), appobs.LaneIdentity{}, false,
	)
	if err != nil {
		return err
	}
	enc.set("sender_session_terminated", correlation, SenderSessionTerminatedPayload{
		Trigger: trigger, Provenance: provenance, Failure: EncodeDiagnosticFailure(event.FailureSnapshot()),
	})
	return nil
}

func (enc *encoder) encodeCatalogStorageObserved(event appobs.CatalogStorageObserved) error {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	usage := event.Usage()
	enc.set("catalog_storage", nil, CatalogStoragePayload{
		Operation: operation,
		Cause:     cause,
		Usage: CatalogUsage{
			ActiveScans: decimal(usage.ActiveScans),
			ScanWork:    decimal(usage.ScanWork),
			Entries:     decimal(usage.Entries),
			MemoryBytes: decimal(usage.MemoryBytes),
			SpillBytes:  decimal(usage.SpillBytes),
		},
		LegacyRootsRemoved: decimal(event.LegacyRootsRemoved()),
	})
	return nil
}

func (enc *encoder) encodeRootPrefetchObserved(event appobs.RootPrefetchObserved) error {
	decision, err := nameOf(event.Decision())
	if err != nil {
		return err
	}
	enc.set("root_prefetch", nil, RootPrefetchPayload{
		Decision:     decision,
		Attempt:      decimal(event.Attempt()),
		EntryCount:   decimal(event.EntryCount()),
		OmittedCount: decimal(event.OmittedCount()),
	})
	return nil
}

func (enc *encoder) encodeProtocolObservationObserved(event appobs.ProtocolObservationObserved) error {
	role, err := nameOf(event.Role())
	if err != nil {
		return err
	}
	var kind string
	if event.RequestKind() != 0 {
		kind, err = nameOf(event.RequestKind())
		if err != nil {
			return err
		}
	} else if _, notStarted := event.Fact().(appobs.ResponseSendNotStartedFact); !notStarted {
		return errInvalidSchemaEvent
	}
	context := ProtocolContext{ObservedAt: event.ObservedAt().UTC().Format(time.RFC3339Nano), Role: role, RequestKind: kind}
	switch fact := event.Fact().(type) {
	case appobs.ProtocolOperationFact:
		return enc.encodeProtocolOperation(event, fact)
	case appobs.SenderContentDecisionFact:
		lane, hasLane := fact.Lane()
		correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), lane, hasLane)
		if err != nil {
			return err
		}
		decision, err := projectSenderContentDecision(fact.Decision())
		if err != nil {
			return err
		}
		enc.set("sender_content_decision", correlation, SenderContentDecisionPayload{ProtocolContext: context, ContentDecision: decision})
	case appobs.ResponseSendNotStartedFact:
		return enc.encodeResponseSend(event, context, "protocol_response_send_not_started", fact.ResponseSequence(), fact.ResponseKind(), fact.Content(), fact.Result())
	case appobs.ResponseSendReturnedFact:
		return enc.encodeResponseSend(event, context, "protocol_response_send_returned", fact.ResponseSequence(), fact.ResponseKind(), fact.Content(), fact.Result())
	case appobs.SendAttemptSettledFact:
		attempt, err := projectSendAttempt(fact.Attempt())
		if err != nil {
			return err
		}
		responseKind, err := nameOf(fact.ResponseKind())
		if err != nil {
			return err
		}
		correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), fact.Attempt().Lane(), true)
		if err != nil {
			return err
		}
		enc.set("protocol_send_attempt_settled", correlation, ProtocolSendAttemptSettledPayload{ProtocolContext: context, ResponseSequence: decimal(fact.ResponseSequence()), ResponseKind: responseKind, Attempt: attempt})
	case appobs.ReceivedProtocolErrorFact:
		content, err := projectProtocolErrorContent(fact.Content())
		if err != nil {
			return err
		}
		correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), fact.Lane(), true)
		if err != nil {
			return err
		}
		enc.set("protocol_error_received", correlation, ProtocolErrorReceivedPayload{ProtocolContext: context, ProtocolError: content})
	default:
		return errInvalidSchemaEvent
	}
	return nil
}
func (enc *encoder) encodeResponseSend(event appobs.ProtocolObservationObserved, context ProtocolContext, name string, sequence uint64, kind appobs.ProtocolMessageKind, content appobs.ProtocolErrorContent, result appobs.ResponseSendResult) error {
	responseKind, err := nameOf(kind)
	if err != nil {
		return err
	}
	projectedContent, err := projectProtocolErrorContent(content)
	if err != nil {
		return err
	}
	projectedResult, err := projectResponseSendResult(result)
	if err != nil {
		return err
	}
	correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), appobs.LaneIdentity{}, false)
	if err != nil {
		return err
	}
	enc.set(name, correlation, ProtocolResponseSendPayload{ProtocolContext: context, ResponseSequence: decimal(sequence), ResponseKind: responseKind, ProtocolError: projectedContent, ResponseResult: projectedResult})
	return nil
}
func projectSendAttempt(value appobs.SendAttemptSnapshot) (SendAttempt, error) {
	outcome, err := nameOf(value.Outcome())
	if err != nil {
		return SendAttempt{}, err
	}
	end, err := nameOf(value.End())
	if err != nil {
		return SendAttempt{}, err
	}
	lane := value.Lane()
	cause, err := nameOf(value.Cause().Kind())
	if err != nil {
		return SendAttempt{}, err
	}
	result := SendAttempt{Cause: SendAttemptCause{Kind: cause, Detail: value.Cause().Detail(), Truncated: value.Cause().Truncated()}, AttemptSequence: decimal(uint64(value.AttemptSequence())), LaneID: lane.ID(), LaneEpoch: lane.Epoch(), PolicyAdmitted: value.PolicyAdmitted(), Settled: value.Settled(), Outcome: outcome, End: end}
	if disposition, ok := value.TransportDisposition(); ok {
		result.TransportDisposition, err = namedPointer(disposition)
	}
	return result, err
}
func projectResponseSendResult(value appobs.ResponseSendResult) (ResponseSendResult, error) {
	evidence, err := nameOf(value.Evidence())
	if err != nil {
		return ResponseSendResult{}, err
	}
	end, err := nameOf(value.End())
	if err != nil {
		return ResponseSendResult{}, err
	}
	cleanup, err := nameOf(value.Cleanup())
	if err != nil {
		return ResponseSendResult{}, err
	}
	result := ResponseSendResult{Started: value.Started(), Evidence: evidence, End: end, Cleanup: cleanup, Attempts: make([]SendAttempt, 0, value.AttemptCount())}
	for i := 0; i < value.AttemptCount(); i++ {
		source, _ := value.Attempt(i)
		attempt, err := projectSendAttempt(source)
		if err != nil {
			return ResponseSendResult{}, err
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	if pending, ok := value.PendingAttemptSequence(); ok {
		result.PendingAttemptSequence = decimalPointer(uint64(pending))
	}
	return result, nil
}
func (enc *encoder) encodeProtocolOperation(
	event appobs.ProtocolObservationObserved, fact appobs.ProtocolOperationFact,
) error {
	role, err := nameOf(event.Role())
	if err != nil {
		return err
	}
	stage, err := nameOf(fact.Stage())
	if err != nil {
		return err
	}
	requestKind, err := nameOf(event.RequestKind())
	if err != nil {
		return err
	}
	cause, err := nameOf(fact.Cause())
	if err != nil {
		return err
	}
	lane, hasLane := fact.Lane()
	correlation, err := projectProtocolCorrelation(
		event.ProtocolSessionID(), event.ProtocolOperationID(), lane, hasLane,
	)
	if err != nil {
		return err
	}
	payload := ProtocolOperationPayload{
		ObservedAt:              event.ObservedAt().UTC().Format(time.RFC3339Nano),
		Role:                    role,
		Stage:                   stage,
		RequestKind:             requestKind,
		ResponseCount:           decimal(fact.ResponseCount()),
		OperationElapsedMS:      decimal(fact.OperationElapsedMillis()),
		UsableLanesAtSelection:  fact.UsableLanesAtSelection(),
		UsableLanesAtSettlement: fact.UsableLanesAtSettlement(),
		Cause:                   cause,
	}
	if wait := fact.BlockWait(); wait.Phase != 0 {
		phase, _ := wait.Phase.Name()
		payload.BlockWait = &BlockWait{Phase: phase, WaitedMS: decimal(wait.WaitedMillis), QueueProgress: decimal(wait.QueueProgress)}
	}
	if scheduling := fact.RequestScheduling(); scheduling.ExpectedMillis != 0 {
		payload.RequestScheduling = &RequestScheduling{
			ExpectedMS:      decimal(scheduling.ExpectedMillis),
			ResponseMS:      decimal(scheduling.ResponseMillis),
			QueuedContentMS: decimal(scheduling.QueuedContentMillis),
			PendingRequests: scheduling.PendingRequests,
		}
	}
	if responseKind, ok := fact.ResponseKind(); ok {
		payload.ResponseKind, err = namedPointer(responseKind)
		if err != nil {
			return err
		}
	}
	outcome, settled, admitted, hasSend := fact.Send()
	if hasSend {
		outcomeName, nameErr := nameOf(outcome)
		if nameErr != nil {
			return nameErr
		}
		payload.Send = &ProtocolSend{
			Settled: settled, Admitted: admitted, Outcome: outcomeName,
		}
	}
	if deadline, ok := fact.DeadlineRemainingMillis(); ok {
		payload.DeadlineRemainingMS = decimalPointer(deadline)
	}
	enc.set("protocol_operation", correlation, payload)
	return nil
}

func projectSenderContentDecision(
	decision appobs.SenderContentDecision,
) (*SenderContentDecision, error) {
	kind, err := nameOf(decision.Kind())
	if err != nil || !decision.Valid() {
		return nil, errInvalidSchemaEvent
	}
	projected := &SenderContentDecision{Kind: kind}
	if id, ok := decision.CapacityDecisionID(); ok {
		encoded := id.Hex()
		projected.CapacityDecisionID = &encoded
	}
	if id, ok := decision.LeaseID(); ok {
		encoded := id.Hex()
		projected.LeaseID = &encoded
	}
	return projected, nil
}

func projectProtocolErrorContent(content appobs.ProtocolErrorContent) (*ProtocolErrorContent, error) {
	if content.IsZero() {
		return nil, nil
	}
	scope, err := nameOf(content.WireScope())
	if err != nil {
		return nil, err
	}
	projected := &ProtocolErrorContent{Scope: scope, Code: content.WireCode(), Retryable: content.Retryable()}
	if retry, ok := content.RetryAfterMillis(); ok {
		projected.RetryAfterMS = new(retry)
	}
	return projected, nil
}

func (enc *encoder) encodeLaneSettlementObserved(
	event appobs.LaneSettlementObserved,
) error {
	route, err := nameOf(event.Route())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(
		event.ProtocolSessionID(), event.Lane(), true,
	)
	if err != nil {
		return err
	}
	enc.set("lane_settlement", correlation, LaneSettlementPayload{
		Route:               route,
		DeliveredBlocks:     decimal(event.DeliveredBlocks()),
		DeliveredBytes:      decimal(event.DeliveredBytes()),
		FailedBlockAttempts: decimal(event.FailedBlockAttempts()),
		ReassignedBlocks:    decimal(event.ReassignedBlocks()),
		Incomplete:          event.Incomplete(),
	})
	return nil
}

func (enc *encoder) encodeObserverLossObserved(event appobs.ObserverLossObserved) error {
	category, err := nameOf(event.Category())
	if err != nil {
		return err
	}
	reason, err := nameOf(event.Reason())
	if err != nil {
		return err
	}
	payload := ObserverLossPayload{Category: category, Reason: reason, Count: decimal(event.Count()), OmittedSamples: decimal(event.OmittedSamples())}
	if sample, ok := event.Rejection(); ok {
		rejection := &ObservationRejectionPayload{Event: sample.Event, Source: sample.Source, Stage: sample.Stage, Field: sample.Field, Rule: sample.Rule, Session: sample.Session, Operation: sample.Operation, Revision: sample.Revision, ResponseSequence: sample.ResponseSequence, AttemptSequence: sample.AttemptSequence, Truncated: sample.Truncated(), OmittedFields: decimal(sample.OmittedFields()), OmittedBytes: decimal(sample.OmittedBytes()), Evidence: make([]RejectionField, 0)}
		for _, field := range sample.Evidence() {
			rejection.Evidence = append(rejection.Evidence, RejectionField{Field: field.Field, Representation: field.Representation, Value: field.Value})
		}
		payload.Rejection = rejection
	}
	enc.set("observer_loss", nil, payload)
	return nil
}

func (enc *encoder) encodeReceiverTerminationObserved(
	event appobs.ReceiverTerminationObserved,
) error {
	authority, err := nameOf(event.TransitionAuthority())
	if err != nil {
		return err
	}
	disposition, err := nameOf(event.Disposition())
	if err != nil {
		return err
	}
	transition, err := nameOf(event.TransitionProvenance())
	if err != nil {
		return err
	}
	consequence, err := nameOf(event.ConsequenceProvenance())
	if err != nil {
		return err
	}
	localStop, err := nameOf(event.LocalStopReason())
	if err != nil {
		return err
	}
	benign, err := namesOf(event.BenignComponents())
	if err != nil {
		return err
	}
	retained, err := namesOf(event.RetainedCauseClasses())
	if err != nil {
		return err
	}
	teardown, err := namesOf(event.TeardownTransitions())
	if err != nil {
		return err
	}
	payload := ReceiverTerminationPayload{
		LocalGeneration:       decimal(event.LocalGeneration()),
		TransitionAuthority:   authority,
		Disposition:           disposition,
		TransitionProvenance:  transition,
		ConsequenceProvenance: consequence,
		LocalStopReason:       localStop,
		DiagnosticsTruncated:  event.DiagnosticsTruncated(),
		BenignComponents:      benign,
		RetainedCauseClasses:  retained,
		TeardownTransitions:   teardown,
		PeerShutdownFailed:    event.PeerShutdownFailed(),
		ChannelDrainFailed:    event.ChannelDrainFailed(),
	}
	if operation, ok := event.OperationID(); ok {
		encoded := EncodeIdentity(operation.Bytes())
		payload.ProtocolOperationID = &encoded
	}
	enc.set("receiver_termination", nil, payload)
	return nil
}
