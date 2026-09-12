package runtrace

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"time"
)

func (visitor *encodeVisitorV4) VisitSenderTerminalSendObserved(
	event clievent.SenderTerminalSendObserved,
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
	visitor.set("sender_terminal_send_observed", correlation, senderTerminalSendPayloadV4{
		Settled:              event.Settled(),
		TransportDisposition: transport,
		Outcome:              outcome,
		Decision:             decision,
	})
	return nil
}

func (visitor *encodeVisitorV4) VisitSenderSessionTerminated(
	event clievent.SenderSessionTerminated,
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
		event.ProtocolSessionID(), clievent.LaneIdentity{}, false,
	)
	if err != nil {
		return err
	}
	visitor.set("sender_session_terminated", correlation, senderSessionTerminatedPayloadV4{
		Trigger: trigger, Provenance: provenance, Failure: projectDiagnosticFailure(event.FailureSnapshot()),
	})
	return nil
}

func (visitor *encodeVisitorV4) VisitCatalogStorageObserved(event clievent.CatalogStorageObserved) error {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	usage := event.Usage()
	visitor.set("catalog_storage", nil, catalogStoragePayloadV4{
		Operation: operation,
		Cause:     cause,
		Usage: catalogUsageV4{
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

func (visitor *encodeVisitorV4) VisitRootPrefetchObserved(event clievent.RootPrefetchObserved) error {
	decision, err := nameOf(event.Decision())
	if err != nil {
		return err
	}
	visitor.set("root_prefetch", nil, rootPrefetchPayloadV4{
		Decision:     decision,
		Attempt:      decimal(event.Attempt()),
		EntryCount:   decimal(event.EntryCount()),
		OmittedCount: decimal(event.OmittedCount()),
	})
	return nil
}

func (visitor *encodeVisitorV4) VisitProtocolObservationObserved(event clievent.ProtocolObservationObserved) error {
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
	} else if _, notStarted := event.Fact().(clievent.ResponseSendNotStartedFact); !notStarted {
		return errInvalidSchemaEvent
	}
	context := protocolContextV4{ObservedAt: event.ObservedAt().UTC().Format(time.RFC3339Nano), Role: role, RequestKind: kind}
	switch fact := event.Fact().(type) {
	case clievent.ProtocolOperationFact:
		return visitor.encodeProtocolOperation(event, fact)
	case clievent.SenderContentDecisionFact:
		lane, hasLane := fact.Lane()
		correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), lane, hasLane)
		if err != nil {
			return err
		}
		decision, err := projectSenderContentDecision(fact.Decision())
		if err != nil {
			return err
		}
		visitor.set("sender_content_decision", correlation, senderContentDecisionPayloadV4{protocolContextV4: context, ContentDecision: decision})
	case clievent.ResponseSendNotStartedFact:
		return visitor.encodeResponseSend(event, context, "protocol_response_send_not_started", fact.ResponseSequence(), fact.ResponseKind(), fact.Content(), fact.Result())
	case clievent.ResponseSendReturnedFact:
		return visitor.encodeResponseSend(event, context, "protocol_response_send_returned", fact.ResponseSequence(), fact.ResponseKind(), fact.Content(), fact.Result())
	case clievent.SendAttemptSettledFact:
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
		visitor.set("protocol_send_attempt_settled", correlation, protocolSendAttemptSettledPayloadV4{protocolContextV4: context, ResponseSequence: decimal(fact.ResponseSequence()), ResponseKind: responseKind, Attempt: attempt})
	case clievent.ReceivedProtocolErrorFact:
		content, err := projectProtocolErrorContent(fact.Content())
		if err != nil {
			return err
		}
		correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), fact.Lane(), true)
		if err != nil {
			return err
		}
		visitor.set("protocol_error_received", correlation, protocolErrorReceivedPayloadV4{protocolContextV4: context, ProtocolError: content})
	default:
		return errInvalidSchemaEvent
	}
	return nil
}
func (visitor *encodeVisitorV4) encodeResponseSend(event clievent.ProtocolObservationObserved, context protocolContextV4, name string, sequence uint64, kind clievent.ProtocolMessageKind, content clievent.ProtocolErrorContent, result clievent.ResponseSendResult) error {
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
	correlation, err := projectProtocolCorrelation(event.ProtocolSessionID(), event.ProtocolOperationID(), clievent.LaneIdentity{}, false)
	if err != nil {
		return err
	}
	visitor.set(name, correlation, protocolResponseSendPayloadV4{protocolContextV4: context, ResponseSequence: decimal(sequence), ResponseKind: responseKind, ProtocolError: projectedContent, ResponseResult: projectedResult})
	return nil
}
func projectSendAttempt(value clievent.SendAttemptSnapshot) (sendAttemptV4, error) {
	outcome, err := nameOf(value.Outcome())
	if err != nil {
		return sendAttemptV4{}, err
	}
	end, err := nameOf(value.End())
	if err != nil {
		return sendAttemptV4{}, err
	}
	lane := value.Lane()
	cause, err := nameOf(value.Cause().Kind())
	if err != nil {
		return sendAttemptV4{}, err
	}
	result := sendAttemptV4{Cause: sendAttemptCauseV4{Kind: cause, Detail: value.Cause().Detail(), Truncated: value.Cause().Truncated()}, AttemptSequence: decimal(uint64(value.AttemptSequence())), LaneID: lane.ID(), LaneEpoch: lane.Epoch(), PolicyAdmitted: value.PolicyAdmitted(), Settled: value.Settled(), Outcome: outcome, End: end}
	if disposition, ok := value.TransportDisposition(); ok {
		result.TransportDisposition, err = namedPointer(disposition)
	}
	return result, err
}
func projectResponseSendResult(value clievent.ResponseSendResult) (responseSendResultV4, error) {
	evidence, err := nameOf(value.Evidence())
	if err != nil {
		return responseSendResultV4{}, err
	}
	end, err := nameOf(value.End())
	if err != nil {
		return responseSendResultV4{}, err
	}
	cleanup, err := nameOf(value.Cleanup())
	if err != nil {
		return responseSendResultV4{}, err
	}
	result := responseSendResultV4{Started: value.Started(), Evidence: evidence, End: end, Cleanup: cleanup, Attempts: make([]sendAttemptV4, 0, value.AttemptCount())}
	for i := 0; i < value.AttemptCount(); i++ {
		source, _ := value.Attempt(i)
		attempt, err := projectSendAttempt(source)
		if err != nil {
			return responseSendResultV4{}, err
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	if pending, ok := value.PendingAttemptSequence(); ok {
		result.PendingAttemptSequence = decimalPointer(uint64(pending))
	}
	return result, nil
}
func (visitor *encodeVisitorV4) encodeProtocolOperation(
	event clievent.ProtocolObservationObserved, fact clievent.ProtocolOperationFact,
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
	payload := protocolOperationPayloadV4{
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
		payload.Send = &protocolSendV4{
			Settled: settled, Admitted: admitted, Outcome: outcomeName,
		}
	}
	if deadline, ok := fact.DeadlineRemainingMillis(); ok {
		payload.DeadlineRemainingMS = decimalPointer(deadline)
	}
	visitor.set("protocol_operation", correlation, payload)
	return nil
}

func projectSenderContentDecision(
	decision clievent.SenderContentDecision,
) (*senderContentDecisionV4, error) {
	kind, err := nameOf(decision.Kind())
	if err != nil || !decision.Valid() {
		return nil, errInvalidSchemaEvent
	}
	projected := &senderContentDecisionV4{Kind: kind}
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

func projectProtocolErrorContent(content clievent.ProtocolErrorContent) (*ProtocolErrorContentV4, error) {
	if content.IsZero() {
		return nil, nil
	}
	scope, err := nameOf(content.WireScope())
	if err != nil {
		return nil, err
	}
	projected := &ProtocolErrorContentV4{Scope: scope, Code: content.WireCode(), Retryable: content.Retryable()}
	if retry, ok := content.RetryAfterMillis(); ok {
		projected.RetryAfterMS = new(retry)
	}
	return projected, nil
}

func (visitor *encodeVisitorV4) VisitLaneSettlementObserved(
	event clievent.LaneSettlementObserved,
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
	visitor.set("lane_settlement", correlation, laneSettlementPayloadV4{
		Route:               route,
		DeliveredBlocks:     decimal(event.DeliveredBlocks()),
		DeliveredBytes:      decimal(event.DeliveredBytes()),
		FailedBlockAttempts: decimal(event.FailedBlockAttempts()),
		ReassignedBlocks:    decimal(event.ReassignedBlocks()),
		Incomplete:          event.Incomplete(),
	})
	return nil
}

func (visitor *encodeVisitorV4) VisitObserverLossObserved(event clievent.ObserverLossObserved) error {
	category, err := nameOf(event.Category())
	if err != nil {
		return err
	}
	reason, err := nameOf(event.Reason())
	if err != nil {
		return err
	}
	payload := observerLossPayloadV4{Category: category, Reason: reason, Count: decimal(event.Count()), OmittedSamples: decimal(event.OmittedSamples())}
	if sample, ok := event.Rejection(); ok {
		rejection := &observationRejectionPayloadV4{Event: sample.Event, Source: sample.Source, Stage: sample.Stage, Field: sample.Field, Rule: sample.Rule, Session: sample.Session, Operation: sample.Operation, Revision: sample.Revision, ResponseSequence: sample.ResponseSequence, AttemptSequence: sample.AttemptSequence, Truncated: sample.Truncated(), OmittedFields: decimal(sample.OmittedFields()), OmittedBytes: decimal(sample.OmittedBytes()), Evidence: make([]rejectionFieldV4, 0)}
		for _, field := range sample.Evidence() {
			rejection.Evidence = append(rejection.Evidence, rejectionFieldV4{Field: field.Field, Representation: field.Representation, Value: field.Value})
		}
		payload.Rejection = rejection
	}
	visitor.set("observer_loss", nil, payload)
	return nil
}

func (visitor *encodeVisitorV4) VisitPlatformSetupObserved(event clievent.PlatformSetupObserved) error {
	visitor.set("platform_setup", nil, platformSetupPayloadV4{State: event.State(), Reason: event.Reason()})
	return nil
}

func (visitor *encodeVisitorV4) VisitReceiverTerminationObserved(
	event clievent.ReceiverTerminationObserved,
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
	payload := receiverTerminationPayloadV4{
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
		encoded := encodeTypedIdentity(operation.Bytes())
		payload.ProtocolOperationID = &encoded
	}
	visitor.set("receiver_termination", nil, payload)
	return nil
}
