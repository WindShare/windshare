package runtrace

import "github.com/windshare/windshare/cmd/wind/internal/clievent"

func (visitor *encodeVisitorV2) VisitFilesystemOutputObserved(event clievent.FilesystemOutputObserved) error {
	visitor.record.Event = "filesystem_output"
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	visitor.record.Operation = new(operation)
	visitor.setFilesystemOutputIdentities(event)
	if err := visitor.setFilesystemOutputAuthority(event); err != nil {
		return err
	}
	visitor.setFilesystemOutputCorrelation(event)
	visitor.setFilesystemOutputCounters(event.Counters())
	return visitor.setFilesystemOutputFailure(event)
}

func (visitor *encodeVisitorV2) setFilesystemOutputIdentities(event clievent.FilesystemOutputObserved) {
	if receiveOperation, ok := event.ReceiveOperationID(); ok {
		visitor.record.ReceiveOperationID = new(receiveOperation.Hex())
	}
	if receiveIntent, ok := event.ReceiveIntentDigest(); ok {
		visitor.record.ReceiveIntentDigest = new(receiveIntent.Hex())
	}
	if outputSession, ok := event.OutputSessionID(); ok {
		visitor.record.OutputSessionID = new(outputSession.Hex())
	}
}

func (visitor *encodeVisitorV2) setFilesystemOutputAuthority(event clievent.FilesystemOutputObserved) error {
	if certification, ok := event.Certification(); ok {
		if err := setNamedRecordField(&visitor.record.FilesystemCertification, certification); err != nil {
			return err
		}
	}
	if disposition, ok := event.RootDisposition(); ok {
		if err := setNamedRecordField(&visitor.record.FilesystemRootDisposition, disposition); err != nil {
			return err
		}
	}
	if err := visitor.setFilesystemNativeLock(event); err != nil {
		return err
	}
	return visitor.setFilesystemRuntimeDecision(event)
}

func (visitor *encodeVisitorV2) setFilesystemNativeLock(event clievent.FilesystemOutputObserved) error {
	if scope, milestone, ok := event.NativeLock(); ok {
		if err := setNamedRecordField(&visitor.record.FilesystemNativeLockScope, scope); err != nil {
			return err
		}
		return setNamedRecordField(&visitor.record.FilesystemNativeLockMilestone, milestone)
	}
	return nil
}

func (visitor *encodeVisitorV2) setFilesystemRuntimeDecision(event clievent.FilesystemOutputObserved) error {
	if component, runtimeOperation, decision, ok := event.RuntimeDecision(); ok {
		if err := setNamedRecordField(&visitor.record.FilesystemRuntimeComponent, component); err != nil {
			return err
		}
		if err := setNamedRecordField(&visitor.record.FilesystemRuntimeOperation, runtimeOperation); err != nil {
			return err
		}
		return setNamedRecordField(&visitor.record.FilesystemRuntimeDecision, decision)
	}
	return nil
}

func setNamedRecordField(target **string, value namedValue) error {
	named, err := namedPointer(value)
	if err != nil {
		return err
	}
	*target = named
	return nil
}

func setOptionalNamedRecordField(target **string, value namedValue) error {
	if _, present := value.Name(); !present {
		return nil
	}
	return setNamedRecordField(target, value)
}

func (visitor *encodeVisitorV2) setFilesystemOutputCorrelation(event clievent.FilesystemOutputObserved) {
	operationID, claimID := event.Correlation()
	if operationID != 0 {
		visitor.record.FilesystemOperationID = decimalPointer(operationID)
	}
	if claimID != 0 {
		visitor.record.FilesystemClaimID = decimalPointer(claimID)
	}
}

func (visitor *encodeVisitorV2) setFilesystemOutputCounters(counters clievent.FilesystemOutputCounters) {
	visitor.record.NodeClaims = decimalPointer(counters.NodeClaims)
	visitor.record.DirectoryClaims = decimalPointer(counters.DirectoryClaims)
	visitor.record.FileClaims = decimalPointer(counters.FileClaims)
	visitor.record.ActiveFileClaims = decimalPointer(counters.ActiveFileClaims)
	visitor.record.ReservedFileSlots = decimalPointer(counters.ReservedFileSlots)
	visitor.record.DirectoryMetadataBytes = decimalPointer(counters.DirectoryMetadataBytes)
	visitor.record.CheckpointRecords = decimalPointer(counters.CheckpointRecords)
}

func (visitor *encodeVisitorV2) setFilesystemOutputFailure(event clievent.FilesystemOutputObserved) error {
	failure, ok := event.Failure()
	if !ok {
		return nil
	}
	stage, reconciliation, nativeClass, _ := event.FailureClassification()
	if err := setNamedRecordField(&visitor.record.FilesystemFailureStage, stage); err != nil {
		return err
	}
	if err := setOptionalNamedRecordField(&visitor.record.FilesystemReconciliationStep, reconciliation); err != nil {
		return err
	}
	if err := setOptionalNamedRecordField(&visitor.record.FilesystemNativeErrorClass, nativeClass); err != nil {
		return err
	}
	return visitor.setFailure(failure)
}

func (visitor *encodeVisitorV2) VisitSenderTerminalObserved(event clievent.SenderTerminalObserved) error {
	visitor.record.Event = "sender_terminal"
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
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.setLane(event.Lane())
	visitor.record.Settled = new(event.Settled())
	visitor.record.TransportDisposition = new(transport)
	visitor.record.Outcome = new(outcome)
	visitor.record.Decision = new(decision)
	return nil
}

func (visitor *encodeVisitorV2) VisitCatalogStorageObserved(event clievent.CatalogStorageObserved) error {
	visitor.record.Event = "catalog_storage"
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	visitor.record.Operation = new(operation)
	visitor.record.Cause = new(cause)
	usage := event.Usage()
	visitor.record.ActiveScans = decimalPointer(usage.ActiveScans)
	visitor.record.ScanWork = decimalPointer(usage.ScanWork)
	visitor.record.Entries = decimalPointer(usage.Entries)
	visitor.record.MemoryBytes = decimalPointer(usage.MemoryBytes)
	visitor.record.SpillBytes = decimalPointer(usage.SpillBytes)
	visitor.record.LegacyRootsRemoved = decimalPointer(event.LegacyRootsRemoved())
	return nil
}

func (visitor *encodeVisitorV2) VisitRootPrefetchObserved(event clievent.RootPrefetchObserved) error {
	visitor.record.Event = "root_prefetch"
	decision, err := nameOf(event.Decision())
	if err != nil {
		return err
	}
	visitor.record.Decision = new(decision)
	visitor.record.RootPrefetchAttempt = decimalPointer(event.Attempt())
	visitor.record.RootPrefetchEntryCount = decimalPointer(event.EntryCount())
	visitor.record.RootPrefetchOmittedCount = decimalPointer(event.OmittedCount())
	return nil
}

func (visitor *encodeVisitorV2) VisitProtocolOperationObserved(event clievent.ProtocolOperationObserved) error {
	visitor.record.Event = "protocol_operation"
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
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.record.ProtocolOperationID = new(event.ProtocolOperationID().Hex())
	visitor.record.ProtocolRole = new(role)
	visitor.record.ProtocolOperationStage = new(stage)
	visitor.record.ProtocolRequestKind = new(requestKind)
	visitor.record.ProtocolOperationCause = new(cause)
	if responseKind, ok := event.ResponseKind(); ok {
		name, nameErr := nameOf(responseKind)
		if nameErr != nil {
			return nameErr
		}
		visitor.record.ProtocolResponseKind = new(name)
	}
	if lane, ok := event.Lane(); ok {
		visitor.setLane(lane)
	}
	outcome, settled, admitted, hasSend := event.Send()
	visitor.record.ProtocolHasSend = new(hasSend)
	if hasSend {
		name, nameErr := nameOf(outcome)
		if nameErr != nil {
			return nameErr
		}
		visitor.record.ProtocolSendOutcome = new(name)
		visitor.record.ProtocolSendSettled = new(settled)
		visitor.record.ProtocolSendAdmitted = new(admitted)
	}
	visitor.record.ProtocolResponseCount = decimalPointer(event.ResponseCount())
	if deadline, ok := event.DeadlineRemainingMillis(); ok {
		visitor.record.ProtocolDeadlineRemainingMS = decimalPointer(deadline)
	}
	visitor.record.ProtocolOperationElapsedMS = decimalPointer(event.OperationElapsedMillis())
	visitor.record.ProtocolUsableLanesAtSelection = decimalPointer(uint64(event.UsableLanesAtSelection()))
	visitor.record.ProtocolUsableLanesAtSettlement = decimalPointer(uint64(event.UsableLanesAtSettlement()))
	if scope, code, retryable, ok := event.OperationError(); ok {
		name, nameErr := nameOf(scope)
		if nameErr != nil {
			return nameErr
		}
		visitor.record.ProtocolErrorScope = new(name)
		visitor.record.ProtocolErrorCode = new(code)
		visitor.record.ProtocolErrorRetryable = new(retryable)
	}
	return nil
}

func (visitor *encodeVisitorV2) VisitLaneSettlementObserved(event clievent.LaneSettlementObserved) error {
	visitor.record.Event = "lane_settlement"
	route, err := nameOf(event.Route())
	if err != nil {
		return err
	}
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.record.LaneRoute = new(route)
	visitor.setLane(event.Lane())
	visitor.record.DeliveredBlocks = decimalPointer(event.DeliveredBlocks())
	visitor.record.DeliveredBytes = decimalPointer(event.DeliveredBytes())
	visitor.record.FailedBlockAttempts = decimalPointer(event.FailedBlockAttempts())
	visitor.record.ReassignedBlocks = decimalPointer(event.ReassignedBlocks())
	visitor.record.Incomplete = new(event.Incomplete())
	return nil
}

func (visitor *encodeVisitorV2) VisitObserverLossObserved(event clievent.ObserverLossObserved) error {
	visitor.record.Event = "observer_loss"
	category, err := nameOf(event.Category())
	if err != nil {
		return err
	}
	reason, err := nameOf(event.Reason())
	if err != nil {
		return err
	}
	visitor.record.ObserverLossCategory = new(category)
	visitor.record.ObserverLossReason = new(reason)
	visitor.record.ObserverLossCount = decimalPointer(event.Count())
	return nil
}

func (visitor *encodeVisitorV2) VisitReceiverTerminationObserved(event clievent.ReceiverTerminationObserved) error {
	visitor.record.Event = "receiver_termination"
	if operation, ok := event.OperationID(); ok {
		visitor.record.ProtocolOperationID = new(operation.Hex())
	}
	visitor.record.ReceiverLocalGeneration = decimalPointer(event.LocalGeneration())
	var err error
	if visitor.record.ReceiverTransitionAuthority, err = namedPointer(event.TransitionAuthority()); err != nil {
		return err
	}
	if visitor.record.ReceiverDisposition, err = namedPointer(event.Disposition()); err != nil {
		return err
	}
	if visitor.record.ReceiverTransitionProvenance, err = namedPointer(event.TransitionProvenance()); err != nil {
		return err
	}
	if visitor.record.ReceiverConsequenceProvenance, err = namedPointer(event.ConsequenceProvenance()); err != nil {
		return err
	}
	if visitor.record.ReceiverLocalStopReason, err = namedPointer(event.LocalStopReason()); err != nil {
		return err
	}
	visitor.record.ReceiverDiagnosticsTruncated = new(event.DiagnosticsTruncated())
	if visitor.record.ReceiverBenignComponents, err = namesOf(event.BenignComponents()); err != nil {
		return err
	}
	if visitor.record.ReceiverRetainedCauseClasses, err = namesOf(event.RetainedCauseClasses()); err != nil {
		return err
	}
	if visitor.record.ReceiverTeardownTransitions, err = namesOf(event.TeardownTransitions()); err != nil {
		return err
	}
	visitor.record.ReceiverPeerShutdownFailed = new(event.PeerShutdownFailed())
	visitor.record.ReceiverChannelDrainFailed = new(event.ChannelDrainFailed())
	return nil
}
