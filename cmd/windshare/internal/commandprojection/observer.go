package commandprojection

import (
	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func ProjectSharingSubject(summary liveshare.SelectedRootSummary) (clievent.SharingSubjectSelected, error) {
	count := summary.SelectedCount()
	if count == 0 {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	var subject clievent.SharingSubject
	var err error
	if count > 1 {
		subject, err = clievent.NewMultipleSubject(count)
	} else {
		root, ok := summary.SingleRoot()
		if !ok || root.Name() == "" {
			return clievent.SharingSubjectSelected{}, ErrInvalidProjection
		}
		name := clievent.NewDisplayName(root.Name())
		switch root.Kind() {
		case liveshare.SelectedRootKindFile:
			size, ok := root.FileSize()
			if !ok {
				return clievent.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = clievent.NewFileSubject(name, size)
		case liveshare.SelectedRootKindDirectory:
			if _, ok := root.FileSize(); ok {
				return clievent.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = clievent.NewDirectorySubject(name)
		default:
			return clievent.SharingSubjectSelected{}, ErrInvalidProjection
		}
	}
	if err != nil {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	event, err := clievent.NewSharingSubjectSelected(subject)
	if err != nil {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectRelayLifecycle(
	command clievent.Command,
	value relayv2.LifecycleTrace,
) (clievent.RelayLifecycleObserved, error) {
	stage, ok := projectRelayStage(value.Stage)
	if !ok {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	retirement, ok := projectRelayRetirement(value.RetirementSource)
	if !ok {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	cause, ok := projectRelayCause(value.Cause)
	if !ok {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	drain, ok := projectRelayCause(value.DrainCause)
	if !ok {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewRelayLifecycleObserved(clievent.RelayLifecycleSpec{
		Command: command, LinkID: value.LinkID, SendOperationID: value.OperationID,
		Stage: stage, Terminal: value.Terminal, Disposition: disposition,
		RetirementSource: retirement, Cause: cause, DrainCause: drain,
	})
	if err != nil {
		return clievent.RelayLifecycleObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectWebRTCLifecycle(
	command clievent.Command,
	value wsrtc.LifecycleTrace,
) (clievent.WebRTCLifecycleObserved, error) {
	operation, ok := projectWebRTCOperation(value.Operation)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	transition, ok := projectWebRTCTransition(value.Transition)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	state, ok := projectChannelState(value.State)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	terminal, ok := projectWebRTCTerminal(value.Terminal)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	cause, ok := projectWebRTCCause(value.Cause)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewWebRTCLifecycleObserved(clievent.WebRTCLifecycleSpec{
		Command: command, ChannelID: value.ChannelID, SendOperationID: value.OperationID,
		Operation: operation, Transition: transition, Disposition: disposition,
		State: state, Terminal: terminal, Cause: cause, Dropped: value.Dropped,
	})
	if err != nil {
		return clievent.WebRTCLifecycleObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderAttempt(
	command clievent.Command,
	value v2peer.SenderAttemptObservation,
) (clievent.PeerAttemptObserved, error) {
	session, err := ProtocolSessionID(value.SessionID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	path, err := PeerPathID(value.PeerPathID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	attempt, err := PeerAttemptID(value.AttemptID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	stage, ok := projectPeerStage(value.Stage)
	if !ok {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	spec := clievent.PeerAttemptSpec{
		Command: command, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: value.SideSequence, ElapsedMillis: value.AttemptElapsedMillis, Stage: stage,
	}
	if value.CandidateCounts != nil {
		spec.Candidates = clievent.CandidateCounts{
			LocalEmitted:   value.CandidateCounts.LocalEmitted,
			RemoteAccepted: value.CandidateCounts.RemoteAccepted,
		}
		spec.HasCandidates = true
	}
	if value.Lane != nil {
		spec.Lane, err = LaneIdentity(*value.Lane)
		if err != nil {
			return clievent.PeerAttemptObserved{}, ErrInvalidProjection
		}
		spec.HasLane = true
	}
	if value.Failure != nil {
		spec.FailureScope, ok = projectPeerFailureScope(value.Failure.Scope)
		if !ok {
			return clievent.PeerAttemptObserved{}, ErrInvalidProjection
		}
		spec.Failure, ok = ProjectPeerErrorCode(value.Failure.TypedCode)
		if !ok {
			return clievent.PeerAttemptObserved{}, ErrInvalidProjection
		}
	}
	event, err := clievent.NewPeerAttemptObserved(spec)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectTransferLifecycle(value transfer.TransferLifecycleTrace) (clievent.TransferLifecycleObserved, error) {
	if value.Interruption != 0 && !value.Interruption.Valid() {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	receiveID, err := ReceiveOperationID(value.ReceiveOperationID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	sessionID, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	jobID, err := TransferJobID(value.TransferJobID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	stage, ok := projectTransferStage(value.Stage)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	progress, err := ProjectProgress(value.Progress)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	selection, ok := projectFileSelection(value.FileSelection)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	fileSettlement, ok := projectFileSettlement(value.FileSettlement)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	treeSettlement, ok := projectTreeSettlement(value.DirectTreeSettlement)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	spec := clievent.TransferLifecycleSpec{
		ReceiveOperation: receiveID, ProtocolSession: sessionID, TransferJob: jobID,
		Stage: stage, Progress: progress, FileSelection: selection,
		FileSettlement: fileSettlement, TreeSettlement: treeSettlement,
	}
	if value.Failed {
		if spec.Failure, ok = ProjectFault(value.Fault); !ok {
			if spec.Failure, ok = ProjectTransferInterruption(value.Interruption); !ok {
				spec.Failure = mustFailure(clievent.FailureUnexpected)
			}
		}
	} else if value.Fault.Valid() || value.Interruption != 0 {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewTransferLifecycleObserved(spec)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectProtocolOperation(
	command clievent.Command,
	value sessionruntime.ProtocolOperationTrace,
) (clievent.ProtocolOperationObserved, error) {
	role, ok := projectProtocolRole(value.Role)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	stage, ok := projectProtocolOperationStage(value.Stage)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	sessionID, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	operationID, err := ProtocolOperationID(value.OperationID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	requestKind, ok := projectProtocolMessageKind(value.RequestKind)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	var responseKind clievent.ProtocolMessageKind
	if value.HasResponse {
		responseKind, ok = projectProtocolMessageKind(value.ResponseKind)
		if !ok {
			return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
		}
	}
	sendOutcome, ok := projectProtocolSendOutcome(value.SendOutcome)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	cause, ok := projectProtocolOperationCause(value.Cause)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	var lane clievent.LaneIdentity
	if value.HasLane {
		lane, err = LaneIdentity(value.Lane)
		if err != nil {
			return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
		}
	}
	event, err := clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command: command, Role: role, Stage: stage,
		ProtocolSession: sessionID, ProtocolOperation: operationID,
		RequestKind: requestKind, ResponseKind: responseKind, HasResponse: value.HasResponse,
		Lane: lane, HasLane: value.HasLane,
		HasSend: value.HasSend, SendSettled: value.SendSettled,
		SendAdmitted: value.SendAdmitted, SendOutcome: sendOutcome,
		ResponseCount:           value.ResponseCount,
		DeadlineRemainingMillis: value.DeadlineRemainingMillis, HasDeadline: value.HasDeadline,
		OperationElapsedMillis:  value.OperationElapsedMillis,
		UsableLanesAtSelection:  value.UsableLanesAtSelection,
		UsableLanesAtSettlement: value.UsableLanesAtSettlement,
		Cause:                   cause,
	})
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectFilesystemOutput(value osfs.FilesystemOutputTrace) (clievent.FilesystemOutputObserved, error) {
	operation, ok := projectFilesystemOperation(value.Operation)
	if !ok {
		return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	var receiveID clievent.ReceiveOperationID
	var err error
	if !value.ReceiveOperationID.IsZero() {
		receiveID, err = ReceiveOperationID(value.ReceiveOperationID)
		if err != nil {
			return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
		}
	}
	var failure clievent.Failure
	if value.Failed {
		if failure, ok = ProjectNormalizedFault(value.FaultDomain, value.NormalizedFaultScope, value.NormalizedFaultCode); !ok {
			failure = mustFailure(clievent.FailureUnexpected)
		}
	} else if value.FaultDomain != 0 || value.NormalizedFaultScope != 0 || value.NormalizedFaultCode != 0 {
		return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewFilesystemOutputObserved(
		receiveID,
		operation,
		clievent.FilesystemOutputCounters{
			NodeClaims: value.NodeClaimCount, DirectoryClaims: value.DirectoryClaimCount,
			FileClaims: value.FileClaimCount, ActiveFileClaims: value.ActiveFileClaimCount,
			ReservedFileSlots:      value.ReservedFileSlotCount,
			DirectoryMetadataBytes: value.DirectoryMetadataBytes,
			CheckpointRecords:      value.CheckpointRecordCount,
		},
		failure,
	)
	if err != nil {
		return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderTerminal(value sessionruntime.SenderTerminalObservation) (clievent.SenderTerminalObserved, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	lane, err := LaneIdentity(value.Lane)
	if err != nil {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	transport, ok := projectSenderTerminalTransport(value.TransportDisposition)
	if !ok {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	outcome, ok := projectSenderTerminalOutcome(value.Outcome)
	if !ok {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	decision, ok := projectSenderTerminalDecision(value.Decision)
	if !ok {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewSenderTerminalObserved(
		session, lane, value.Settled, transport, outcome, decision,
	)
	if err != nil {
		return clievent.SenderTerminalObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectCatalogStorage(value liveshare.CatalogStorageTrace) (clievent.CatalogStorageObserved, error) {
	operation, ok := projectCatalogStorageOperation(value.Operation)
	if !ok {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	cause, ok := projectCatalogStorageCause(value.Cause)
	if !ok {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewCatalogStorageObserved(
		operation,
		cause,
		clievent.CatalogUsage{
			ActiveScans: value.RecoveredUsage.ActiveScans, ScanWork: value.RecoveredUsage.ScanWork,
			Entries: value.RecoveredUsage.Entries, MemoryBytes: value.RecoveredUsage.MemoryBytes,
			SpillBytes: value.RecoveredUsage.SpillBytes,
		},
		value.LegacyRootsRemoved,
	)
	if err != nil {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectRootPrefetch(value liveshare.RootPrefetchTrace) (clievent.RootPrefetchObserved, error) {
	decision, ok := projectRootPrefetchDecision(value.Decision)
	if !ok {
		return clievent.RootPrefetchObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewRootPrefetchObserved(
		decision, value.Attempt, value.EntryCount, value.OmittedCount,
	)
	if err != nil {
		return clievent.RootPrefetchObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectOptionalDisposition(value framechannel.SendDisposition) (clievent.SendDisposition, bool) {
	switch value {
	case 0:
		return 0, true
	case framechannel.SendAccepted:
		return clievent.SendAccepted, true
	case framechannel.SendRejected:
		return clievent.SendRejected, true
	case framechannel.SendRetired:
		return clievent.SendRetired, true
	default:
		return 0, false
	}
}

func projectChannelState(value framechannel.ChannelState) (clievent.ChannelState, bool) {
	switch value {
	case framechannel.Connecting:
		return clievent.ChannelConnecting, true
	case framechannel.Open:
		return clievent.ChannelOpen, true
	case framechannel.Closed:
		return clievent.ChannelClosed, true
	default:
		return 0, false
	}
}
