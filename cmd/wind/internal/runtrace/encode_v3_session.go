package runtrace

import "github.com/windshare/windshare/cmd/wind/internal/clievent"

func (visitor *encodeVisitorV3) VisitSenderTerminalSendObserved(
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
	visitor.set("sender_terminal_send_observed", correlation, senderTerminalSendPayloadV3{
		Settled:              event.Settled(),
		TransportDisposition: transport,
		Outcome:              outcome,
		Decision:             decision,
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitSenderSessionTerminated(
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
	visitor.set("sender_session_terminated", correlation, senderSessionTerminatedPayloadV3{
		Trigger: trigger, Provenance: provenance,
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitCatalogStorageObserved(event clievent.CatalogStorageObserved) error {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	usage := event.Usage()
	visitor.set("catalog_storage", nil, catalogStoragePayloadV3{
		Operation: operation,
		Cause:     cause,
		Usage: catalogUsageV3{
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

func (visitor *encodeVisitorV3) VisitRootPrefetchObserved(event clievent.RootPrefetchObserved) error {
	decision, err := nameOf(event.Decision())
	if err != nil {
		return err
	}
	visitor.set("root_prefetch", nil, rootPrefetchPayloadV3{
		Decision:     decision,
		Attempt:      decimal(event.Attempt()),
		EntryCount:   decimal(event.EntryCount()),
		OmittedCount: decimal(event.OmittedCount()),
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitProtocolOperationObserved(
	event clievent.ProtocolOperationObserved,
) error {
	role, err := nameOf(event.Role())
	if err != nil {
		return err
	}
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	requestKind, err := nameOf(event.RequestKind())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	lane, hasLane := event.Lane()
	correlation, err := projectProtocolCorrelation(
		event.ProtocolSessionID(), event.ProtocolOperationID(), lane, hasLane,
	)
	if err != nil {
		return err
	}
	payload := protocolOperationPayloadV3{
		Role:                    role,
		Stage:                   stage,
		RequestKind:             requestKind,
		ResponseCount:           decimal(event.ResponseCount()),
		OperationElapsedMS:      decimal(event.OperationElapsedMillis()),
		UsableLanesAtSelection:  event.UsableLanesAtSelection(),
		UsableLanesAtSettlement: event.UsableLanesAtSettlement(),
		Cause:                   cause,
	}
	if responseKind, ok := event.ResponseKind(); ok {
		payload.ResponseKind, err = namedPointer(responseKind)
		if err != nil {
			return err
		}
	}
	outcome, settled, admitted, hasSend := event.Send()
	if hasSend {
		outcomeName, nameErr := nameOf(outcome)
		if nameErr != nil {
			return nameErr
		}
		payload.Send = &protocolSendV3{
			Settled: settled, Admitted: admitted, Outcome: outcomeName,
		}
	}
	if deadline, ok := event.DeadlineRemainingMillis(); ok {
		payload.DeadlineRemainingMS = decimalPointer(deadline)
	}
	if failure, ok := event.Failure(); ok {
		payload.ProtocolFailure, err = projectProtocolFailure(failure)
		if err != nil {
			return err
		}
	}
	if decision, ok := event.ContentDecision(); ok {
		payload.ContentDecision, err = projectSenderContentDecision(decision)
		if err != nil {
			return err
		}
	}
	visitor.set("protocol_operation", correlation, payload)
	return nil
}

func projectSenderContentDecision(
	decision clievent.SenderContentDecision,
) (*senderContentDecisionV3, error) {
	kind, err := nameOf(decision.Kind())
	if err != nil || !decision.Valid() {
		return nil, errInvalidSchemaEvent
	}
	projected := &senderContentDecisionV3{Kind: kind}
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

func projectProtocolFailure(failure clievent.ProtocolFailure) (*ProtocolFailureV1, error) {
	if !failure.Valid() {
		return nil, errInvalidSchemaEvent
	}
	requestKind, err := nameOf(failure.RequestKind())
	if err != nil {
		return nil, err
	}
	wireScope, err := nameOf(failure.WireScope())
	if err != nil {
		return nil, err
	}
	lane, hasLane := failure.Lane()
	correlation, err := projectProtocolCorrelation(
		failure.ProtocolSessionID(), failure.ProtocolOperationID(), lane, hasLane,
	)
	if err != nil || correlation == nil ||
		correlation.ProtocolSessionID == "" || correlation.ProtocolOperationID == "" {
		return nil, errInvalidSchemaEvent
	}
	projected := &ProtocolFailureV1{
		RequestKind: requestKind,
		WireScope:   wireScope,
		WireCode:    failure.WireCode(),
		Retryable:   failure.Retryable(),
		Correlation: *correlation,
	}
	settlement := failure.Settlement()
	kind, err := nameOf(settlement.Kind())
	if err != nil {
		return nil, err
	}
	switch settlement.Kind() {
	case clievent.ProtocolFailureReceivedAuthenticated:
		projected.Settlement = receivedAuthenticatedSettlementV1{Kind: kind}
		if retryAfter, ok := failure.RetryAfterMillis(); ok {
			projected.RetryAfterMS = new(retryAfter)
		}
	case clievent.ProtocolFailureResponseSend:
		response, ok := settlement.ResponseSend()
		if !ok {
			return nil, errInvalidSchemaEvent
		}
		outcome, nameErr := nameOf(response.Outcome)
		if nameErr != nil {
			return nil, nameErr
		}
		projected.Settlement = responseSendSettlementV1{
			Kind:     kind,
			Admitted: response.Admitted,
			Settled:  response.Settled,
			Outcome:  outcome,
		}
	default:
		return nil, errInvalidSchemaEvent
	}
	return projected, nil
}

func (visitor *encodeVisitorV3) VisitLaneSettlementObserved(
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
	visitor.set("lane_settlement", correlation, laneSettlementPayloadV3{
		Route:               route,
		DeliveredBlocks:     decimal(event.DeliveredBlocks()),
		DeliveredBytes:      decimal(event.DeliveredBytes()),
		FailedBlockAttempts: decimal(event.FailedBlockAttempts()),
		ReassignedBlocks:    decimal(event.ReassignedBlocks()),
		Incomplete:          event.Incomplete(),
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitObserverLossObserved(event clievent.ObserverLossObserved) error {
	category, err := nameOf(event.Category())
	if err != nil {
		return err
	}
	reason, err := nameOf(event.Reason())
	if err != nil {
		return err
	}
	visitor.set("observer_loss", nil, observerLossPayloadV3{
		Category: category, Reason: reason, Count: decimal(event.Count()),
	})
	return nil
}

func (visitor *encodeVisitorV3) VisitPlatformSetupObserved(event clievent.PlatformSetupObserved) error {
	visitor.set("platform_setup", nil, platformSetupPayloadV3{State: event.State(), Reason: event.Reason()})
	return nil
}

func (visitor *encodeVisitorV3) VisitReceiverTerminationObserved(
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
	payload := receiverTerminationPayloadV3{
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
