package clievent

import (
	"errors"
	"testing"
	"time"

	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	"github.com/windshare/windshare/core/diagnosticerror"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestApplicationFactsAndCommandResultsValidate(t *testing.T) {
	failure, _ := appobs.NewFailure(appobs.FailureRelayTransport)
	authority, _ := appobs.NewRelayAuthority(appobs.RelayWSS, "relay.example", 443)
	subject, _ := appobs.NewDirectorySubject(appobs.NewDisplayName("photos"))
	subjectEvent, _ := appobs.NewSharingSubjectSelected(subject)
	relayConnected, _ := NewRelayConnected(CommandGet, authority)
	recovery, _ := NewRelayRecovering(CommandShare, authority, 1, appobs.RelayRecoveryStarted, appobs.Failure{})
	path, _ := appobs.NewContentPathSelected(appobs.ContentPathDirect)
	fallback, _ := NewFallback(CommandGet, appobs.TransportWebRTC, appobs.TransportRelay, failure)
	receiveID, _ := appobs.NewReceiveOperationID(bytes16(1))
	jobID, _ := appobs.NewTransferJobID(bytes16(2))
	snapshot, _ := appobs.NewProgressSnapshot(appobs.ProgressSpec{Discovery: appobs.DiscoveryOpen, CountersExact: true})
	progress, _ := appobs.NewTransferProgress(receiveID, jobID, snapshot)
	warning, _ := NewWarning(CommandGet, failure)
	failed, _ := NewCommandFailed(CommandGet, ExitNetwork, failure)
	transferResult, _ := NewTransferResult(TransferResultSpec{
		Status: ResultSuccess, ExitCode: ExitSuccess, Drift: DriftNone,
		Destination: appobs.NewDisplayPath("result"), CountersExact: true,
	})
	settled, _ := NewTransferSettled(transferResult)
	shareResult, _ := NewShareResult(ShareResultSpec{ExitCode: ExitSuccess})
	stopped, _ := NewSharingStopped(shareResult)
	incomplete, _ := NewTraceIncomplete(CommandGet, TraceIncompleteWriter, TraceLoss{})
	sessionID, _ := appobs.NewProtocolSessionID(bytes16(3))
	lane, _ := appobs.NewLaneIdentity(2, 1)
	adopted, _ := NewLaneAdopted(CommandGet, sessionID, lane, appobs.TransportWebRTC)
	relayLifecycle, _ := appobs.NewRelayLifecycleObserved(appobs.RelayLifecycleSpec{
		Workflow: appobs.WorkflowReceive, LinkID: 1, SendOperationID: 1, Stage: appobs.RelayLinkClosed,
		RetirementSource: appobs.RelayRetirementLocalClose, Cause: appobs.RelayCauseNone, DrainCause: appobs.RelayCauseNone,
	})
	webRTCLifecycle, _ := appobs.NewWebRTCLifecycleObserved(appobs.WebRTCLifecycleSpec{
		Workflow: appobs.WorkflowReceive, ChannelID: 1, Operation: appobs.WebRTCChannel,
		Transition: appobs.WebRTCClosedClean, State: appobs.ChannelClosed,
		Terminal: appobs.WebRTCTerminalNone, Cause: appobs.WebRTCCauseNone,
	})
	peerPath, _ := appobs.NewPeerPathID(bytes16(4))
	peerAttemptID, _ := appobs.NewPeerAttemptID(bytes16(5))
	peerAttempt, _ := appobs.NewPeerAttemptObserved(appobs.PeerAttemptSpec{
		Workflow: appobs.WorkflowShare, Session: sessionID, PeerPath: peerPath,
		Attempt: peerAttemptID, Sequence: 1, Stage: appobs.PeerAttemptStarted,
	})
	transferLifecycle, _ := appobs.NewTransferLifecycleObserved(appobs.TransferLifecycleSpec{
		ReceiveOperation: receiveID, ProtocolSession: sessionID, TransferJob: jobID,
		Stage: appobs.TransferDiscoveryStarted, Progress: snapshot,
		FileSelection: appobs.FileSelectionNone, FileSettlement: appobs.FileSettlementNone,
		TreeSettlement: appobs.TreeSettlementNone,
	})
	filesystemOutput, _ := fsobs.NewFilesystemOutputObserved(fsobs.FilesystemOutputSpec{
		ReceiveOperation: receiveID, Operation: fsobs.FilesystemRuntimeDecision,
	})
	senderTerminalSend, _ := appobs.NewSenderTerminalSendObserved(
		sessionID, lane, true, appobs.SenderTerminalSendAccepted,
		appobs.SenderTerminalSendDelivered, appobs.SenderTerminalSendDecisionDelivered,
	)
	senderSessionTerminated, _ := appobs.NewSenderSessionTerminated(
		sessionID, appobs.SenderSessionTerminalGracefulStop, appobs.SenderSessionTerminalNormalStop,
		diagnosticerror.Snapshot{},
	)
	catalogStorage, _ := appobs.NewCatalogStorageObserved(
		appobs.CatalogStorageRecovered, appobs.CatalogStorageCauseNone, appobs.CatalogUsage{}, 0,
	)
	rootPrefetch, _ := appobs.NewRootPrefetchObserved(appobs.RootPrefetchCommitted, 1, 2, 3)
	protocolOperationID, _ := appobs.NewProtocolOperationID(bytes16(6))
	protocolOperation, _ := appobs.NewProtocolOperationObserved(appobs.ProtocolOperationSpec{ObservedAt: time.Unix(1, 0),
		Workflow: appobs.WorkflowReceive, Role: appobs.ProtocolRoleReceiver,
		Stage:           appobs.ProtocolOperationReceiverFailed,
		ProtocolSession: sessionID, ProtocolOperation: protocolOperationID,
		RequestKind: appobs.ProtocolMessageReleaseLease,
		Lane:        lane, HasLane: true, Cause: appobs.ProtocolOperationCauseDeadline,
	})
	laneSettlement, _ := appobs.NewLaneSettlementObserved(appobs.LaneSettlementSpec{Session: sessionID, Route: appobs.LaneRouteDirect, Lane: lane})
	observerLoss, _ := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{Workflow: appobs.WorkflowReceive, Category: appobs.ObserverLossCommandAdapter, Reason: appobs.ObserverLossEventContract, Count: 1})
	receiverTermination, _ := appobs.NewReceiverTerminationObserved(appobs.ReceiverTerminationSpec{
		LocalGeneration: 1, TransitionAuthority: appobs.ReceiverTerminalLocal,
		Disposition: appobs.ReceiverFallbackAllowed, TransitionProvenance: appobs.ReceiverProvenanceLocalExplicitStop,
		ConsequenceProvenance: appobs.ReceiverProvenanceLocalExplicitStop, LocalStopReason: appobs.ReceiverLocalStopCaller,
	})

	tests := []struct {
		name  string
		event Event
	}{
		{"ready", appobs.NewReady()},
		{"sharing_subject", subjectEvent},
		{"relay_connected", relayConnected},
		{"relay_recovering", recovery},
		{"content_path", path},
		{"fallback", fallback},
		{"progress", progress},
		{"warning", warning},
		{"failed", failed},
		{"settled", settled},
		{"stopped", stopped},
		{"trace_incomplete", incomplete},
		{"lane_adopted", adopted},
		{"relay_lifecycle", relayLifecycle},
		{"webrtc_lifecycle", webRTCLifecycle},
		{"peer_attempt", peerAttempt},
		{"transfer_lifecycle", transferLifecycle},
		{"filesystem_output", filesystemOutput},
		{"sender_terminal_send", senderTerminalSend},
		{"sender_session_terminated", senderSessionTerminated},
		{"catalog_storage", catalogStorage},
		{"root_prefetch", rootPrefetch},
		{"protocol_operation", protocolOperation},
		{"lane_settlement", laneSettlement},
		{"observer_loss", observerLoss},
		{"receiver_termination", receiverTermination},
	}
	for _, test := range tests {
		test.name = ""
		if err := test.event.Validate(); err != nil {
			t.Fatalf("%T dispatched %q, want %q (err=%v)", test.event, test.name, test.name, err)
		}
	}
	if err := (appobs.RelayConnected{}).Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero relay event dispatched: %v", err)
	}
	if err := (appobs.RootPrefetchObserved{}).Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero root-prefetch event dispatched: %v", err)
	}
}

func TestRootPrefetchEventPreservesOnlyDecisionAndBoundedCounters(t *testing.T) {
	event, err := appobs.NewRootPrefetchObserved(appobs.RootPrefetchCommitted, 7, 11, 2)
	if err != nil {
		t.Fatal(err)
	}
	if CommandOf(event) != CommandShare || event.Level() != appobs.LevelDebug ||
		event.Decision() != appobs.RootPrefetchCommitted || event.Attempt() != 7 ||
		event.EntryCount() != 11 || event.OmittedCount() != 2 {
		t.Fatalf("root-prefetch event = %#v", event)
	}
	for _, test := range []struct {
		name     string
		decision appobs.RootPrefetchDecision
		attempt  uint64
		entries  uint64
		omitted  uint64
	}{
		{"unknown decision", appobs.RootPrefetchDecision(255), 1, 0, 0},
		{"missing attempt", appobs.RootPrefetchAttemptStarted, 0, 0, 0},
		{"non-commit entries", appobs.RootPrefetchScanFailed, 1, 1, 0},
		{"non-commit omissions", appobs.RootPrefetchStopped, 0, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := appobs.NewRootPrefetchObserved(test.decision, test.attempt, test.entries, test.omitted); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid root-prefetch event error = %v", err)
			}
		})
	}
	if _, err := appobs.NewRootPrefetchObserved(appobs.RootPrefetchStopped, 0, 0, 0); err != nil {
		t.Fatalf("pre-attempt stop rejected: %v", err)
	}
}

func bytes16(seed byte) []byte {
	value := make([]byte, appobs.IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}
