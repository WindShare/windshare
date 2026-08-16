package runtrace

import (
	"errors"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

var errInvalidSchemaEvent = errors.New("event cannot be represented by trace schema v1")

type namedValue interface {
	Name() (string, bool)
}

type encodeVisitorV1 struct{ record *recordV1 }

func encodeV1(runID string, metadata entryMetadata, event clievent.Event) (recordV1, error) {
	record, err := baseRecordV1(runID, metadata, event.Command(), event.Level(), "pending")
	if err != nil {
		return recordV1{}, err
	}
	visitor := &encodeVisitorV1{record: &record}
	if err := event.Accept(visitor); err != nil {
		return recordV1{}, err
	}
	if record.Event == "pending" {
		return recordV1{}, errInvalidSchemaEvent
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

func (visitor *encodeVisitorV1) VisitReady(clievent.Ready) error {
	visitor.record.Event = "ready"
	return nil
}

func (visitor *encodeVisitorV1) VisitSharingSubjectSelected(event clievent.SharingSubjectSelected) error {
	visitor.record.Event = "sharing_subject_selected"
	subject := event.Subject()
	kind, err := nameOf(subject.Kind())
	if err != nil {
		return err
	}
	visitor.record.SubjectKind = new(kind)
	visitor.record.SelectedItems = decimalPointer(subject.SelectedItems())
	if subject.Kind() == clievent.SharingFile {
		visitor.record.FileBytes = decimalPointer(subject.FileBytes())
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitRelayConnected(event clievent.RelayConnected) error {
	visitor.record.Event = "relay_connected"
	return visitor.setRelayAuthority(event.Authority())
}

func (visitor *encodeVisitorV1) VisitRelayRecovering(event clievent.RelayRecovering) error {
	visitor.record.Event = "relay_recovering"
	if err := visitor.setRelayAuthority(event.Authority()); err != nil {
		return err
	}
	state, err := nameOf(event.State())
	if err != nil {
		return err
	}
	visitor.record.Attempt = new(event.Attempt())
	visitor.record.State = new(state)
	if failure, ok := event.Failure(); ok {
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitContentPathSelected(event clievent.ContentPathSelected) error {
	visitor.record.Event = "content_path_selected"
	path, err := nameOf(event.Path())
	if err != nil {
		return err
	}
	visitor.record.ContentPath = new(path)
	return nil
}

func (visitor *encodeVisitorV1) VisitFallback(event clievent.Fallback) error {
	visitor.record.Event = "fallback"
	from, err := nameOf(event.From())
	if err != nil {
		return err
	}
	to, err := nameOf(event.To())
	if err != nil {
		return err
	}
	visitor.record.FromTransport = new(from)
	visitor.record.ToTransport = new(to)
	return visitor.setFailure(event.Failure())
}

func (visitor *encodeVisitorV1) VisitTransferProgress(event clievent.TransferProgress) error {
	visitor.record.Event = "transfer_progress"
	visitor.record.ReceiveOperationID = new(event.ReceiveOperationID().Hex())
	visitor.record.TransferJobID = new(event.TransferJobID().Hex())
	return visitor.setProgress(event.Snapshot())
}

func (visitor *encodeVisitorV1) VisitWarning(event clievent.Warning) error {
	visitor.record.Event = "warning"
	return visitor.setFailure(event.Failure())
}

func (visitor *encodeVisitorV1) VisitCommandFailed(event clievent.CommandFailed) error {
	visitor.record.Event = "command_failed"
	exitCode, ok := event.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	visitor.record.ExitCode = new(exitCode)
	return visitor.setFailure(event.Failure())
}

func (visitor *encodeVisitorV1) VisitTransferSettled(event clievent.TransferSettled) error {
	visitor.record.Event = "transfer_settled"
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
	visitor.record.ResultStatus = new(status)
	visitor.record.ExitCode = new(exitCode)
	visitor.record.Drift = new(drift)
	visitor.record.ResultElapsedMS = new(result.Elapsed().Milliseconds())
	visitor.record.DestinationAdjusted = new(result.DestinationAdjusted())
	visitor.setOutcomes(result.Files())
	visitor.record.DirectoryFailures = decimalPointer(result.DirectoryFailures())
	visitor.record.OmittedDiagnostics = decimalPointer(result.OmittedDiagnostics())
	visitor.record.PublishedBytes = decimalPointer(result.PublishedBytes())
	visitor.record.CountersExact = new(result.CountersExact())
	if failure, ok := result.Failure(); ok {
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitSharingStopped(event clievent.SharingStopped) error {
	visitor.record.Event = "sharing_stopped"
	result := event.Result()
	exitCode, ok := result.ExitCode().ProcessCode()
	if !ok {
		return errInvalidSchemaEvent
	}
	visitor.record.ExitCode = new(exitCode)
	visitor.record.ResultElapsedMS = new(result.Elapsed().Milliseconds())
	visitor.record.StoppedCleanly = new(result.StoppedCleanly())
	if failure, ok := result.Failure(); ok {
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitTraceIncomplete(event clievent.TraceIncomplete) error {
	visitor.record.Event = "trace_incomplete"
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	visitor.record.TraceIncompleteCause = new(cause)
	visitor.record.LifecycleDropped = decimalPointer(event.LifecycleDrops())
	visitor.record.ProgressDropped = decimalPointer(event.ProgressDrops())
	return nil
}

func (visitor *encodeVisitorV1) VisitLaneAdopted(event clievent.LaneAdopted) error {
	visitor.record.Event = "lane_adopted"
	transport, err := nameOf(event.Transport())
	if err != nil {
		return err
	}
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.record.Transport = new(transport)
	visitor.setLane(event.Lane())
	return nil
}

func (visitor *encodeVisitorV1) VisitRelayLifecycleObserved(event clievent.RelayLifecycleObserved) error {
	visitor.record.Event = "relay_lifecycle"
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	retirement, err := nameOf(event.RetirementSource())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	drain, err := nameOf(event.DrainCause())
	if err != nil {
		return err
	}
	visitor.record.RelayLinkID = decimalPointer(event.LinkID())
	visitor.record.RelaySendOperationID = decimalPointer(event.SendOperationID())
	visitor.record.Stage = new(stage)
	visitor.record.Terminal = new(event.Terminal())
	visitor.record.RetirementSource = new(retirement)
	visitor.record.Cause = new(cause)
	visitor.record.DrainCause = new(drain)
	if disposition, ok := event.Disposition(); ok {
		name, err := nameOf(disposition)
		if err != nil {
			return err
		}
		visitor.record.Disposition = new(name)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitWebRTCLifecycleObserved(event clievent.WebRTCLifecycleObserved) error {
	visitor.record.Event = "webrtc_lifecycle"
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	transition, err := nameOf(event.Transition())
	if err != nil {
		return err
	}
	state, err := nameOf(event.State())
	if err != nil {
		return err
	}
	terminal, err := nameOf(event.Terminal())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	visitor.record.WebRTCChannelID = decimalPointer(event.ChannelID())
	visitor.record.WebRTCSendOperationID = decimalPointer(event.SendOperationID())
	visitor.record.Operation = new(operation)
	visitor.record.Transition = new(transition)
	visitor.record.State = new(state)
	visitor.record.TerminalState = new(terminal)
	visitor.record.Cause = new(cause)
	if disposition, ok := event.Disposition(); ok {
		name, err := nameOf(disposition)
		if err != nil {
			return err
		}
		visitor.record.Disposition = new(name)
	}
	if event.Dropped() != 0 {
		visitor.record.Dropped = decimalPointer(event.Dropped())
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitPeerAttemptObserved(event clievent.PeerAttemptObserved) error {
	visitor.record.Event = "peer_attempt"
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.record.PeerPathID = new(event.PeerPathID().Hex())
	visitor.record.PeerAttemptID = new(event.PeerAttemptID().Hex())
	visitor.record.AttemptSequence = decimalPointer(event.Sequence())
	visitor.record.AttemptElapsedMS = decimalPointer(event.ElapsedMillis())
	visitor.record.Stage = new(stage)
	if candidates, ok := event.Candidates(); ok {
		visitor.record.CandidatesLocalEmitted = new(candidates.LocalEmitted)
		visitor.record.CandidatesRemoteAccepted = new(candidates.RemoteAccepted)
	}
	if lane, ok := event.Lane(); ok {
		visitor.setLane(lane)
	}
	if scope, failure, ok := event.Failure(); ok {
		scopeName, err := nameOf(scope)
		if err != nil {
			return err
		}
		visitor.record.FailureScope = new(scopeName)
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitTransferLifecycleObserved(event clievent.TransferLifecycleObserved) error {
	visitor.record.Event = "transfer_lifecycle"
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	selection, err := nameOf(event.FileSelection())
	if err != nil {
		return err
	}
	fileSettlement, err := nameOf(event.FileSettlement())
	if err != nil {
		return err
	}
	treeSettlement, err := nameOf(event.TreeSettlement())
	if err != nil {
		return err
	}
	visitor.record.ReceiveOperationID = new(event.ReceiveOperationID().Hex())
	visitor.record.ProtocolSessionID = new(event.ProtocolSessionID().Hex())
	visitor.record.TransferJobID = new(event.TransferJobID().Hex())
	visitor.record.Stage = new(stage)
	visitor.record.FileSelection = new(selection)
	visitor.record.FileSettlement = new(fileSettlement)
	visitor.record.TreeSettlement = new(treeSettlement)
	if err := visitor.setProgress(event.Progress()); err != nil {
		return err
	}
	if failure, ok := event.Failure(); ok {
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitFilesystemOutputObserved(event clievent.FilesystemOutputObserved) error {
	visitor.record.Event = "filesystem_output"
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	visitor.record.Operation = new(operation)
	if receiveOperation, ok := event.ReceiveOperationID(); ok {
		visitor.record.ReceiveOperationID = new(receiveOperation.Hex())
	}
	counters := event.Counters()
	visitor.record.NodeClaims = decimalPointer(counters.NodeClaims)
	visitor.record.DirectoryClaims = decimalPointer(counters.DirectoryClaims)
	visitor.record.FileClaims = decimalPointer(counters.FileClaims)
	visitor.record.ActiveFileClaims = decimalPointer(counters.ActiveFileClaims)
	visitor.record.ReservedFileSlots = decimalPointer(counters.ReservedFileSlots)
	visitor.record.DirectoryMetadataBytes = decimalPointer(counters.DirectoryMetadataBytes)
	visitor.record.CheckpointRecords = decimalPointer(counters.CheckpointRecords)
	if failure, ok := event.Failure(); ok {
		return visitor.setFailure(failure)
	}
	return nil
}

func (visitor *encodeVisitorV1) VisitSenderTerminalObserved(event clievent.SenderTerminalObserved) error {
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

func (visitor *encodeVisitorV1) VisitCatalogStorageObserved(event clievent.CatalogStorageObserved) error {
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

func (visitor *encodeVisitorV1) VisitRootPrefetchObserved(event clievent.RootPrefetchObserved) error {
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

func (visitor *encodeVisitorV1) VisitProtocolOperationObserved(event clievent.ProtocolOperationObserved) error {
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
	return nil
}

func (visitor *encodeVisitorV1) setRelayAuthority(authority clievent.RelayAuthority) error {
	scheme, err := nameOf(authority.Scheme())
	if err != nil || !authority.Valid() {
		return errInvalidSchemaEvent
	}
	visitor.record.RelayScheme = new(scheme)
	visitor.record.RelayHost = new(authority.Host())
	visitor.record.RelayPort = new(authority.Port())
	return nil
}

func (visitor *encodeVisitorV1) setLane(lane clievent.LaneIdentity) {
	visitor.record.LaneID = new(lane.ID())
	visitor.record.LaneEpoch = new(lane.Epoch())
}

func (visitor *encodeVisitorV1) setFailure(failure clievent.Failure) error {
	code, err := nameOf(failure.Code())
	if err != nil {
		return err
	}
	messageKey, ok := failure.MessageKey()
	if !ok {
		return errInvalidSchemaEvent
	}
	message, err := nameOf(messageKey)
	if err != nil {
		return err
	}
	visitor.record.FailureCode = new(code)
	visitor.record.MessageKey = new(message)
	if fault, ok := failure.Fault(); ok {
		domain, err := nameOf(fault.Domain())
		if err != nil {
			return err
		}
		scope, err := nameOf(fault.Scope())
		if err != nil {
			return err
		}
		visitor.record.FaultDomain = new(domain)
		visitor.record.FaultScope = new(scope)
		visitor.record.FaultCode = new(fault.Code())
	}
	if retryAfter, ok := failure.RetryAfterMillis(); ok {
		visitor.record.RetryAfterMS = decimalPointer(retryAfter)
	}
	return nil
}

func (visitor *encodeVisitorV1) setProgress(snapshot clievent.ProgressSnapshot) error {
	discovery, err := nameOf(snapshot.Discovery())
	if err != nil || !snapshot.Valid() {
		return errInvalidSchemaEvent
	}
	visitor.record.Discovery = new(discovery)
	visitor.record.CountersExact = new(snapshot.CountersExact())
	visitor.record.DiscoveredFiles = decimalPointer(snapshot.DiscoveredFiles())
	visitor.record.DiscoveredBytes = decimalPointer(snapshot.DiscoveredBytes())
	visitor.record.PublishedFiles = decimalPointer(snapshot.PublishedFiles())
	visitor.record.PublishedBytes = decimalPointer(snapshot.PublishedBytes())
	visitor.record.VerifiedBytes = decimalPointer(snapshot.VerifiedBytes())
	visitor.record.NewlyVerifiedBytes = decimalPointer(snapshot.NewlyVerifiedBytes())
	visitor.setOutcomes(snapshot.FileOutcomes())
	return nil
}

func (visitor *encodeVisitorV1) setOutcomes(outcomes clievent.FileOutcomes) {
	visitor.record.DownloadedFiles = decimalPointer(outcomes.DownloadedFiles)
	visitor.record.ResumedFiles = decimalPointer(outcomes.ResumedFiles)
	visitor.record.PausedFiles = decimalPointer(outcomes.PausedFiles)
	visitor.record.CollisionFiles = decimalPointer(outcomes.CollisionFiles)
	visitor.record.ItemBlockedFiles = decimalPointer(outcomes.ItemBlockedFiles)
	visitor.record.FailedFiles = decimalPointer(outcomes.FailedFiles)
	visitor.record.ModifiedTimeWarnings = decimalPointer(outcomes.ModifiedTimeWarnings)
}
