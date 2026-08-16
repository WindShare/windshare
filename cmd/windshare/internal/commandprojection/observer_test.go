package commandprojection

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestObserverEnumProjectionIsExhaustiveAndRejectsUnknownValues(t *testing.T) {
	assertClosedProjection(t, "relay stage", []relayv2.LifecycleStage{
		relayv2.LifecycleTerminalReserved, relayv2.LifecycleSendAdmitted,
		relayv2.LifecycleSendRejected, relayv2.LifecycleSendRolledBack,
		relayv2.LifecycleRetirementDeferred, relayv2.LifecycleRetired,
		relayv2.LifecycleTerminalSettled, relayv2.LifecycleLinkRetiring,
		relayv2.LifecycleLinkClosed,
	}, relayv2.LifecycleStage("unknown"), projectRelayStage)
	assertClosedProjection(t, "relay retirement", []relayv2.LifecycleRetirementSource{
		"", relayv2.LifecycleRetirementLocalClose, relayv2.LifecycleRetirementTerminal,
		relayv2.LifecycleRetirementRelaySession, relayv2.LifecycleRetirementLinkClose,
		relayv2.LifecycleRetirementLinkFailure, relayv2.LifecycleRetirementIngressFailure,
	}, relayv2.LifecycleRetirementSource("unknown"), projectRelayRetirement)
	assertClosedProjection(t, "relay cause", []relayv2.LifecycleCause{
		relayv2.LifecycleCauseNone, relayv2.LifecycleCauseCanceled,
		relayv2.LifecycleCauseDeadline, relayv2.LifecycleCauseFrameBounds,
		relayv2.LifecycleCauseEgressOverflow, relayv2.LifecycleCauseIngressOverflow,
		relayv2.LifecycleCauseSessionRetired, relayv2.LifecycleCauseProtocol,
		relayv2.LifecycleCauseClosed, relayv2.LifecycleCauseTransport,
	}, relayv2.LifecycleCause("unknown"), projectRelayCause)
	assertClosedProjection(t, "send disposition", []framechannel.SendDisposition{
		0, framechannel.SendAccepted, framechannel.SendRejected, framechannel.SendRetired,
	}, framechannel.SendDisposition(255), projectOptionalDisposition)
	assertClosedProjection(t, "channel state", []framechannel.ChannelState{
		framechannel.Connecting, framechannel.Open, framechannel.Closed,
	}, framechannel.ChannelState(255), projectChannelState)

	assertClosedProjection(t, "webrtc operation", []wsrtc.LifecycleOperation{
		wsrtc.LifecycleOperationChannel, wsrtc.LifecycleOperationSend,
		wsrtc.LifecycleOperationSendTerminal,
	}, wsrtc.LifecycleOperation("unknown"), projectWebRTCOperation)
	assertClosedProjection(t, "webrtc transition", []wsrtc.LifecycleTransition{
		wsrtc.LifecycleTransitionSendAccepted, wsrtc.LifecycleTransitionSendRejected,
		wsrtc.LifecycleTransitionSendRetired, wsrtc.LifecycleTransitionRemoteTerminalReserved,
		wsrtc.LifecycleTransitionTerminationPending, wsrtc.LifecycleTransitionClosedClean,
		wsrtc.LifecycleTransitionClosedFailed, wsrtc.LifecycleTransitionTraceDropped,
	}, wsrtc.LifecycleTransition("unknown"), projectWebRTCTransition)
	assertClosedProjection(t, "webrtc terminal", []wsrtc.LifecycleTerminalState{
		wsrtc.LifecycleTerminalNone, wsrtc.LifecycleTerminalLocalPending,
		wsrtc.LifecycleTerminalRemotePending,
	}, wsrtc.LifecycleTerminalState("unknown"), projectWebRTCTerminal)
	assertClosedProjection(t, "webrtc cause", []wsrtc.LifecycleCause{
		wsrtc.LifecycleCauseNone, wsrtc.LifecycleCauseCanceled, wsrtc.LifecycleCauseDeadline,
		wsrtc.LifecycleCauseNotOpen, wsrtc.LifecycleCauseNaturalRetirement,
		wsrtc.LifecycleCauseRemoteClosed, wsrtc.LifecycleCauseTerminalUnacknowledged,
		wsrtc.LifecycleCausePeerProtocol, wsrtc.LifecycleCauseTransport,
		wsrtc.LifecycleCauseOther,
	}, wsrtc.LifecycleCause("unknown"), projectWebRTCCause)

	assertClosedProjection(t, "peer stage", []v2peer.SenderAttemptStage{
		v2peer.SenderAttemptStarted, v2peer.SenderAttemptOfferReceived,
		v2peer.SenderAttemptAnswerCreated, v2peer.SenderAttemptAnswerSent,
		v2peer.SenderAttemptDataChannelOpen, v2peer.SenderAttemptLaneAdmissionStarted,
		v2peer.SenderAttemptAdmitted, v2peer.SenderAttemptFailed,
	}, v2peer.SenderAttemptStage("unknown"), projectPeerStage)
	assertClosedProjection(t, "peer failure scope", []v2peer.AttemptFailureScope{
		v2peer.AttemptFailureScopeAttempt, v2peer.AttemptFailureScopeSession,
	}, v2peer.AttemptFailureScope("unknown"), projectPeerFailureScope)

	assertClosedProjection(t, "protocol role", []protocolsession.Role{
		protocolsession.RoleReceiver, protocolsession.RoleSender,
	}, protocolsession.Role(255), projectProtocolRole)
	assertClosedProjection(t, "protocol operation stage", []sessionruntime.ProtocolOperationStage{
		sessionruntime.ProtocolOperationReceiverCompleted,
		sessionruntime.ProtocolOperationReceiverFailed,
		sessionruntime.ProtocolOperationReceiverEnded,
		sessionruntime.ProtocolOperationSenderRequestReceived,
		sessionruntime.ProtocolOperationSenderResponseSettled,
	}, sessionruntime.ProtocolOperationStage(255), projectProtocolOperationStage)
	assertClosedProjection(t, "protocol message kind", []protocolsession.MessageKind{
		protocolsession.MessageListChildren, protocolsession.MessageCatalogResult,
		protocolsession.MessageOpenRevisions, protocolsession.MessageOpenResults,
		protocolsession.MessageRenewLease, protocolsession.MessageReleaseLease,
		protocolsession.MessageRequestBlocks, protocolsession.MessageBlockFragment,
		protocolsession.MessageCancel, protocolsession.MessageOperationError,
		protocolsession.MessageSessionTerminal, protocolsession.MessageLaneAttach,
		protocolsession.MessageScanProgress, protocolsession.MessageOperationComplete,
		protocolsession.MessageLeaseResult, protocolsession.MessagePeerOffer,
		protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate,
	}, protocolsession.MessageKind(255), projectProtocolMessageKind)
	assertClosedProjection(t, "protocol send outcome", []protocolsession.SendOutcome{
		protocolsession.SendOutcomeUnknown,
		protocolsession.SendOutcomeDelivered,
		protocolsession.SendOutcomeDropped,
	}, protocolsession.SendOutcome(255), projectProtocolSendOutcome)
	assertClosedProjection(t, "protocol operation cause", []sessionruntime.ProtocolOperationCause{
		sessionruntime.ProtocolOperationCauseNone,
		sessionruntime.ProtocolOperationCauseCanceled,
		sessionruntime.ProtocolOperationCauseDeadline,
		sessionruntime.ProtocolOperationCauseRuntimeClosed,
		sessionruntime.ProtocolOperationCauseLaneUnavailable,
		sessionruntime.ProtocolOperationCauseWriterStopped,
		sessionruntime.ProtocolOperationCauseOperationClosed,
		sessionruntime.ProtocolOperationCauseProtocolFailure,
	}, sessionruntime.ProtocolOperationCause(255), projectProtocolOperationCause)

	assertClosedProjection(t, "transfer stage", []transfer.TransferLifecycleStage{
		transfer.TransferDiscoveryStarted, transfer.TransferGenerationCommitted,
		transfer.TransferDiscoveryCompleted, transfer.TransferAdmissionStarted,
		transfer.TransferAdmissionCompleted, transfer.TransferDirectoryAdmitted,
		transfer.TransferDirectoryFinalized, transfer.TransferFileEnqueued,
		transfer.TransferFileStarted, transfer.TransferFileAdmitted,
		transfer.TransferFileFirstWrite, transfer.TransferFileSettled,
		transfer.TransferJobSettled,
	}, transfer.TransferLifecycleStage(255), projectTransferStage)
	assertClosedProjection(t, "file selection", []transfer.FileSelectionDecision{
		0, transfer.FileSelectionInherited, transfer.FileSelectionNodeOverride,
		transfer.FileSelectionCatalogPathTarget,
	}, transfer.FileSelectionDecision(255), projectFileSelection)
	assertClosedProjection(t, "file settlement", []transfer.FileSettlementKind{
		0, transfer.FilePublished, transfer.FilePaused, transfer.FileCollision,
		transfer.FileItemBlocked, transfer.FileFailed,
	}, transfer.FileSettlementKind(255), projectFileSettlement)
	assertClosedProjection(t, "tree settlement", []transfer.DirectTreeSettlementKind{
		0, transfer.DirectTreeSettlementSuccess, transfer.DirectTreeSettlementPartial,
		transfer.DirectTreeSettlementPaused, transfer.DirectTreeSettlementFailed,
	}, transfer.DirectTreeSettlementKind(255), projectTreeSettlement)

	assertClosedProjection(t, "filesystem operation", []osfs.FilesystemOutputTraceOperation{
		osfs.TraceFilesystemCertified, osfs.TraceFeatureProbeCompleted,
		osfs.TraceCheckpointNamespaceOpened, osfs.TraceNativeLock,
		osfs.TraceSessionOpened, osfs.TraceCheckpointReconciled,
		osfs.TraceRuntimeDecision,
	}, osfs.FilesystemOutputTraceOperation(255), projectFilesystemOperation)
	assertClosedProjection(t, "sender terminal transport", []sessionruntime.SenderTerminalTransportDisposition{
		sessionruntime.SenderTerminalTransportAccepted,
		sessionruntime.SenderTerminalTransportNotReached,
		sessionruntime.SenderTerminalTransportUnsettled,
		sessionruntime.SenderTerminalTransportRejected,
		sessionruntime.SenderTerminalTransportRetired,
	}, sessionruntime.SenderTerminalTransportDisposition("unknown"), projectSenderTerminalTransport)
	assertClosedProjection(t, "sender terminal outcome", []sessionruntime.SenderTerminalOutcome{
		sessionruntime.SenderTerminalOutcomeDelivered,
		sessionruntime.SenderTerminalOutcomeDropped,
		sessionruntime.SenderTerminalOutcomeUnknown,
	}, sessionruntime.SenderTerminalOutcome("invalid"), projectSenderTerminalOutcome)
	assertClosedProjection(t, "sender terminal decision", []sessionruntime.SenderTerminalDecision{
		sessionruntime.SenderTerminalDecisionDelivered,
		sessionruntime.SenderTerminalDecisionNaturalRetirement,
		sessionruntime.SenderTerminalDecisionFailed,
	}, sessionruntime.SenderTerminalDecision("invalid"), projectSenderTerminalDecision)

	assertClosedProjection(t, "catalog storage operation", []liveshare.CatalogStorageOperation{
		liveshare.CatalogStorageCreating, liveshare.CatalogStorageCreated,
		liveshare.CatalogStorageRecovering, liveshare.CatalogStorageRecovered,
		liveshare.CatalogStorageBudgetRejected, liveshare.CatalogStorageCleaning,
		liveshare.CatalogStorageCleaned,
	}, liveshare.CatalogStorageOperation(255), projectCatalogStorageOperation)
	assertClosedProjection(t, "catalog storage cause", []liveshare.CatalogStorageCause{
		liveshare.CatalogStorageCauseNone, liveshare.CatalogStorageCauseCanceled,
		liveshare.CatalogStorageCauseDeadlineExceeded,
		liveshare.CatalogStorageCauseBudgetExceeded,
		liveshare.CatalogStorageCauseUnexpected,
	}, liveshare.CatalogStorageCause(255), projectCatalogStorageCause)
	assertClosedProjection(t, "root prefetch decision", []liveshare.RootPrefetchDecision{
		liveshare.RootPrefetchAttemptStarted, liveshare.RootPrefetchYieldedToDemand,
		liveshare.RootPrefetchRetryScheduled, liveshare.RootPrefetchCommitted,
		liveshare.RootPrefetchBudgetFailed, liveshare.RootPrefetchScanFailed,
		liveshare.RootPrefetchStopped,
	}, liveshare.RootPrefetchDecision(255), projectRootPrefetchDecision)
}

func TestLifecycleProjectionCopiesOnlyWhitelistedFacts(t *testing.T) {
	relay, err := ProjectRelayLifecycle(clievent.CommandShare, relayv2.LifecycleTrace{
		LinkID: 9, RelaySessionID: v2.RelaySessionID{0x91}, OperationID: 10,
		Stage: relayv2.LifecycleLinkClosed, RetirementSource: relayv2.LifecycleRetirementRelaySession,
		Cause: relayv2.LifecycleCauseClosed, DrainCause: relayv2.LifecycleCauseTransport,
	})
	if err != nil || relay.LinkID() != 9 || relay.SendOperationID() != 10 ||
		relay.Stage() != clievent.RelayLinkClosed || relay.RetirementSource() != clievent.RelayRetirementSession {
		t.Fatalf("relay projection = %#v, err %v", relay, err)
	}

	webRTC, err := ProjectWebRTCLifecycle(clievent.CommandGet, wsrtc.LifecycleTrace{
		ChannelID: 12, Operation: wsrtc.LifecycleOperationChannel,
		Transition: wsrtc.LifecycleTransitionTraceDropped, State: framechannel.Closed,
		Terminal: wsrtc.LifecycleTerminalRemotePending, Cause: wsrtc.LifecycleCauseOther,
		Dropped: 3,
	})
	if err != nil || webRTC.ChannelID() != 12 || webRTC.Dropped() != 3 ||
		webRTC.Transition() != clievent.WebRTCTraceDropped {
		t.Fatalf("webrtc projection = %#v, err %v", webRTC, err)
	}

	sessionID := sourceProtocolSessionID(t, 0x21)
	peerPath := v2signal.PeerPathID{0x31, 0x32}
	attemptID := v2signal.AttemptID{0x41, 0x42}
	lane := sessionruntime.LaneIdentity{ID: 7, Epoch: 2}
	peer, err := ProjectSenderAttempt(clievent.CommandShare, v2peer.SenderAttemptObservation{
		SessionID: sessionID, PeerPathID: peerPath, AttemptID: attemptID,
		SideSequence: 4, AttemptElapsedMillis: 120, Stage: v2peer.SenderAttemptAdmitted,
		CandidateCounts: &v2peer.SenderCandidateCounts{LocalEmitted: 2, RemoteAccepted: 1},
		Lane:            &lane,
		SelectedPair: &v2peer.PionSelectedPairEvidence{
			Local:  v2peer.PionCandidateEvidence{Address: "198.51.100.7", Protocol: "udp"},
			Remote: v2peer.PionCandidateEvidence{Address: "203.0.113.9", Protocol: "udp"},
		},
	})
	if err != nil || peer.PeerPathID().Hex() != fmt.Sprintf("%x", peerPath) ||
		peer.PeerAttemptID().Hex() != fmt.Sprintf("%x", attemptID) {
		t.Fatalf("peer projection = %#v, err %v", peer, err)
	}
	assertProjectionOmits(t, peer, "198.51.100.7", "203.0.113.9")

	failedPeer, err := ProjectSenderAttempt(clievent.CommandShare, v2peer.SenderAttemptObservation{
		SessionID: sessionID, PeerPathID: peerPath, AttemptID: attemptID,
		SideSequence: 5, Stage: v2peer.SenderAttemptFailed,
		Failure: &v2peer.SenderAttemptFailure{
			Scope: v2peer.AttemptFailureScopeSession, TypedCode: v2peer.TypedPeerErrorAdmission,
			Message:   "provider-message-SECRET",
			Operation: &v2peer.PeerOperationFailure{Code: 99, Message: "operation-message-SECRET"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, failure, ok := failedPeer.Failure()
	if !ok || failure.Code() != clievent.FailurePeerAdmission {
		t.Fatalf("peer failure = %#v, present %v", failure, ok)
	}
	assertProjectionOmits(t, failedPeer, "provider-message-SECRET", "operation-message-SECRET")
}

func TestProtocolOperationProjectionPreservesCorrelationAndClosedDiagnostics(t *testing.T) {
	sessionID := sourceProtocolSessionID(t, 0x81)
	operationID, err := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{0x82}, protocolsession.IdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := ProjectProtocolOperation(clievent.CommandGet, sessionruntime.ProtocolOperationTrace{
		Stage:             sessionruntime.ProtocolOperationReceiverFailed,
		Role:              protocolsession.RoleReceiver,
		ProtocolSessionID: sessionID, OperationID: operationID,
		RequestKind: protocolsession.MessageReleaseLease,
		Lane:        sessionruntime.LaneIdentity{ID: 2, Epoch: 1}, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome:             protocolsession.SendOutcomeDelivered,
		DeadlineRemainingMillis: 30_000, HasDeadline: true,
		OperationElapsedMillis: 30_000,
		UsableLanesAtSelection: 2, UsableLanesAtSettlement: 2,
		Cause: sessionruntime.ProtocolOperationCauseDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.ProtocolSessionID().Hex() != fmt.Sprintf("%x", sessionID) ||
		event.ProtocolOperationID().Hex() != fmt.Sprintf("%x", operationID) ||
		event.RequestKind() != clievent.ProtocolMessageReleaseLease ||
		event.Cause() != clievent.ProtocolOperationCauseDeadline {
		t.Fatalf("protocol operation projection = %#v", event)
	}
	if _, err := ProjectProtocolOperation(clievent.CommandShare, sessionruntime.ProtocolOperationTrace{
		Stage:             sessionruntime.ProtocolOperationReceiverFailed,
		Role:              protocolsession.RoleReceiver,
		ProtocolSessionID: sessionID, OperationID: operationID,
		RequestKind: protocolsession.MessageReleaseLease,
		Cause:       sessionruntime.ProtocolOperationCauseDeadline,
	}); err != ErrInvalidProjection {
		t.Fatalf("role/command mismatch error = %v", err)
	}
}

func TestCoreObserverProjectionPreservesCorrelationAndDropsAuthoritySecrets(t *testing.T) {
	receiveID := sourceReceiveOperationID(t, 0x51)
	sessionID := sourceProtocolSessionID(t, 0x52)
	jobID := sourceTransferJobID(t, 0x53)
	progress := transfer.ReceiveProgressSnapshot{
		DiscoveredFiles: 2, DiscoveredBytes: 100, PublishedFiles: 1, PublishedBytes: 60,
		VerifiedBytes: 80, NewlyVerifiedBytes: 70,
		FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1},
		Discovery:    transfer.DiscoveryOpen, CountersExact: true,
	}
	transferEvent, err := ProjectTransferLifecycle(transfer.TransferLifecycleTrace{
		Stage: transfer.TransferFileSettled, ReceiveOperationID: receiveID,
		ProtocolSessionID: sessionID, TransferJobID: jobID,
		ReceiveIntentDigest: transfer.ReceiveIntentDigest{0x61},
		OutputSessionID:     transfer.OutputSessionID{0x62},
		FileSelection:       transfer.FileSelectionInherited,
		FileSettlement:      transfer.FilePublished, Progress: progress,
	})
	if err != nil || transferEvent.ReceiveOperationID().Hex() != fmt.Sprintf("%x", receiveID) ||
		transferEvent.ProtocolSessionID().Hex() != fmt.Sprintf("%x", sessionID) ||
		transferEvent.TransferJobID().Hex() != fmt.Sprintf("%x", jobID) {
		t.Fatalf("transfer projection = %#v, err %v", transferEvent, err)
	}
	assertProjectionOmits(t, transferEvent, fmt.Sprintf("%x", transfer.ReceiveIntentDigest{0x61}), fmt.Sprintf("%x", transfer.OutputSessionID{0x62}))
	canceled, err := ProjectTransferLifecycle(transfer.TransferLifecycleTrace{
		Stage: transfer.TransferDiscoveryCompleted, ReceiveOperationID: receiveID,
		ProtocolSessionID: sessionID, TransferJobID: jobID,
		Discovery: transfer.DiscoveryFailed, Progress: progress,
		Interruption: transfer.TransferInterruptionCanceled, Failed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	canceledFailure, present := canceled.Failure()
	if !present || canceledFailure.Code() != clievent.FailureCanceled {
		t.Fatalf("canceled lifecycle=%#v failure=%#v present=%t", canceled, canceledFailure, present)
	}

	filesystemEvent, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{
		Operation: osfs.TraceRuntimeDecision, ReceiveOperationID: receiveID,
		ReceiveIntentDigest: transfer.ReceiveIntentDigest{0x71},
		SessionID:           transfer.OutputSessionID{0x72}, OperationID: 73, ClaimID: 74,
		NodeClaimCount: 1, DirectoryClaimCount: 2, FileClaimCount: 3,
		ActiveFileClaimCount: 4, ReservedFileSlotCount: 5,
		DirectoryMetadataBytes: 6, CheckpointRecordCount: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	projectedReceive, present := filesystemEvent.ReceiveOperationID()
	if !present || projectedReceive.Hex() != fmt.Sprintf("%x", receiveID) ||
		filesystemEvent.Counters().CheckpointRecords != 7 {
		t.Fatalf("filesystem projection = %#v", filesystemEvent)
	}
	assertProjectionOmits(t, filesystemEvent, fmt.Sprintf("%x", transfer.ReceiveIntentDigest{0x71}), fmt.Sprintf("%x", transfer.OutputSessionID{0x72}))
	withoutReceive, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{Operation: osfs.TraceFilesystemCertified})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := withoutReceive.ReceiveOperationID(); present {
		t.Fatal("pre-binding filesystem trace invented receive-operation correlation")
	}

	terminal, err := ProjectSenderTerminal(sessionruntime.SenderTerminalObservation{
		ProtocolSessionID: sessionID, Lane: sessionruntime.LaneIdentity{ID: 8, Epoch: 3},
		Settled: true, TransportDisposition: sessionruntime.SenderTerminalTransportAccepted,
		Outcome:  sessionruntime.SenderTerminalOutcomeDelivered,
		Decision: sessionruntime.SenderTerminalDecisionDelivered,
	})
	if err != nil || terminal.ProtocolSessionID().Hex() != fmt.Sprintf("%x", sessionID) ||
		terminal.Lane().ID() != 8 || !terminal.Settled() {
		t.Fatalf("terminal projection = %#v, err %v", terminal, err)
	}

	shareSecret, err := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{0x81}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	storage, err := ProjectCatalogStorage(liveshare.CatalogStorageTrace{
		Operation: liveshare.CatalogStorageRecovered, Cause: liveshare.CatalogStorageCauseNone,
		ShareInstance: shareSecret, RecoveredUsage: catalog.ResourceUsage{
			ActiveScans: 1, ScanWork: 2, Entries: 3, MemoryBytes: 4, SpillBytes: 5,
		}, LegacyRootsRemoved: 6,
	})
	if err != nil || storage.Usage().SpillBytes != 5 || storage.LegacyRootsRemoved() != 6 {
		t.Fatalf("storage projection = %#v, err %v", storage, err)
	}
	assertProjectionOmits(t, storage, fmt.Sprintf("%x", shareSecret))

	directorySecret, err := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{0x82}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	generationSecret, err := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{0x83}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	prefetch, err := ProjectRootPrefetch(liveshare.RootPrefetchTrace{
		Decision: liveshare.RootPrefetchCommitted, ShareInstance: shareSecret,
		DirectoryID: directorySecret, Generation: generationSecret,
		Attempt: 7, EntryCount: 11, OmittedCount: 2,
	})
	if err != nil || prefetch.Decision() != clievent.RootPrefetchCommitted ||
		prefetch.Attempt() != 7 || prefetch.EntryCount() != 11 || prefetch.OmittedCount() != 2 {
		t.Fatalf("root-prefetch projection = %#v, err %v", prefetch, err)
	}
	assertProjectionOmits(
		t, prefetch, fmt.Sprintf("%x", shareSecret),
		fmt.Sprintf("%x", directorySecret), fmt.Sprintf("%x", generationSecret),
	)
	if _, err := ProjectRootPrefetch(liveshare.RootPrefetchTrace{
		Decision: liveshare.RootPrefetchScanFailed, Attempt: 1, EntryCount: 1,
	}); err == nil {
		t.Fatal("projected root-prefetch counters for a non-commit decision")
	}
}

func TestSharingSubjectProjectionUsesFrozenHumanDisplayFact(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "selected.bin")
	if err := os.WriteFile(path, []byte("selected-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{
		Paths: []string{path}, Relays: []string{"ws://127.0.0.1:8484"},
		ChunkSize: catalog.MinChunkSize,
		CatalogStorage: liveshare.CatalogStorageFactoryFunc(func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error) {
			return catalog.NewMemoryCatalogBackend(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	projected, err := ProjectSharingSubject(sender.SelectedRootSummary())
	if err != nil {
		t.Fatal(err)
	}
	subject := projected.Subject()
	if subject.Kind() != clievent.SharingFile || subject.Name().Text() != filepath.Base(path) {
		t.Fatalf("sharing subject = %#v", subject)
	}
	if size := subject.FileBytes(); size != uint64(len("selected-content")) {
		t.Fatalf("sharing file size = %d", size)
	}
	if _, err := ProjectSharingSubject(liveshare.SelectedRootSummary{}); err == nil {
		t.Fatal("empty selected-root summary was projected")
	}
}

func assertClosedProjection[Source comparable, Target any](
	t *testing.T,
	name string,
	values []Source,
	unknown Source,
	project func(Source) (Target, bool),
) {
	t.Helper()
	for _, value := range values {
		if _, ok := project(value); !ok {
			t.Fatalf("%s rejected supported value %v", name, value)
		}
	}
	if _, ok := project(unknown); ok {
		t.Fatalf("%s accepted unknown value %v", name, unknown)
	}
}

func assertProjectionOmits(t *testing.T, projected any, forbidden ...string) {
	t.Helper()
	encoded := fmt.Sprintf("%#v", projected)
	for _, value := range forbidden {
		if value != "" && strings.Contains(encoded, value) {
			t.Fatalf("projection retained forbidden value %q: %s", value, encoded)
		}
	}
}

func sourceReceiveOperationID(t *testing.T, marker byte) receivecontract.OperationID {
	t.Helper()
	value, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{marker}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func sourceProtocolSessionID(t *testing.T, marker byte) protocolsession.ProtocolSessionID {
	t.Helper()
	value, err := protocolsession.ProtocolSessionIDFromBytes(bytes.Repeat([]byte{marker}, protocolsession.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func sourceTransferJobID(t *testing.T, marker byte) transfer.TransferJobID {
	t.Helper()
	value, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{marker}, transfer.TransferJobIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
