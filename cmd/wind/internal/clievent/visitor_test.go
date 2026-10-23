package clievent

import (
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnosticerror"
	appobs "github.com/windshare/windshare/engine/observation"
)

type exhaustiveVisitor struct{ visited string }

func (visitor *exhaustiveVisitor) VisitEngineTaskObserved(EngineTaskObserved) error {
	return visitor.mark("engine_task")
}

func (visitor *exhaustiveVisitor) VisitNativeConnectivityObserved(NativeConnectivityObserved) error {
	return nil
}

func (visitor *exhaustiveVisitor) VisitPlatformSetupObserved(PlatformSetupObserved) error {
	return visitor.mark("platform_setup")
}

func (visitor *exhaustiveVisitor) mark(name string) error { visitor.visited = name; return nil }
func (visitor *exhaustiveVisitor) VisitReady(Ready) error { return visitor.mark("ready") }
func (visitor *exhaustiveVisitor) VisitSharingSubjectSelected(SharingSubjectSelected) error {
	return visitor.mark("sharing_subject")
}
func (visitor *exhaustiveVisitor) VisitRelayConnected(RelayConnected) error {
	return visitor.mark("relay_connected")
}
func (visitor *exhaustiveVisitor) VisitRelayAvailability(RelayAvailability) error {
	return visitor.mark("relay_availability")
}
func (visitor *exhaustiveVisitor) VisitRelayRecovering(RelayRecovering) error {
	return visitor.mark("relay_recovering")
}
func (visitor *exhaustiveVisitor) VisitContentPathSelected(ContentPathSelected) error {
	return visitor.mark("content_path")
}
func (visitor *exhaustiveVisitor) VisitFallback(Fallback) error {
	return visitor.mark("fallback")
}
func (visitor *exhaustiveVisitor) VisitTransferProgress(TransferProgress) error {
	return visitor.mark("progress")
}
func (visitor *exhaustiveVisitor) VisitWarning(Warning) error { return visitor.mark("warning") }
func (visitor *exhaustiveVisitor) VisitCommandFailed(CommandFailed) error {
	return visitor.mark("failed")
}
func (visitor *exhaustiveVisitor) VisitTransferSettled(TransferSettled) error {
	return visitor.mark("settled")
}
func (visitor *exhaustiveVisitor) VisitSharingStopped(SharingStopped) error {
	return visitor.mark("stopped")
}
func (visitor *exhaustiveVisitor) VisitTraceIncomplete(TraceIncomplete) error {
	return visitor.mark("trace_incomplete")
}
func (visitor *exhaustiveVisitor) VisitLaneAdopted(LaneAdopted) error {
	return visitor.mark("lane_adopted")
}
func (visitor *exhaustiveVisitor) VisitRelayLifecycleObserved(RelayLifecycleObserved) error {
	return visitor.mark("relay_lifecycle")
}
func (visitor *exhaustiveVisitor) VisitWebRTCLifecycleObserved(WebRTCLifecycleObserved) error {
	return visitor.mark("webrtc_lifecycle")
}
func (visitor *exhaustiveVisitor) VisitPeerAttemptObserved(PeerAttemptObserved) error {
	return visitor.mark("peer_attempt")
}
func (visitor *exhaustiveVisitor) VisitTransferLifecycleObserved(TransferLifecycleObserved) error {
	return visitor.mark("transfer_lifecycle")
}
func (visitor *exhaustiveVisitor) VisitFilesystemOutputObserved(FilesystemOutputObserved) error {
	return visitor.mark("filesystem_output")
}
func (visitor *exhaustiveVisitor) VisitSenderTerminalSendObserved(SenderTerminalSendObserved) error {
	return visitor.mark("sender_terminal_send")
}
func (visitor *exhaustiveVisitor) VisitSenderSessionTerminated(SenderSessionTerminated) error {
	return visitor.mark("sender_session_terminated")
}
func (visitor *exhaustiveVisitor) VisitCatalogStorageObserved(CatalogStorageObserved) error {
	return visitor.mark("catalog_storage")
}
func (visitor *exhaustiveVisitor) VisitRootPrefetchObserved(RootPrefetchObserved) error {
	return visitor.mark("root_prefetch")
}
func (visitor *exhaustiveVisitor) VisitSenderCapacityObserved(SenderCapacityObserved) error {
	return visitor.mark("sender_capacity")
}
func (visitor *exhaustiveVisitor) VisitSenderRevisionObserved(SenderRevisionObserved) error {
	return visitor.mark("sender_revision")
}
func (visitor *exhaustiveVisitor) VisitProtocolObservationObserved(ProtocolObservationObserved) error {
	return visitor.mark("protocol_operation")
}
func (visitor *exhaustiveVisitor) VisitLaneSettlementObserved(LaneSettlementObserved) error {
	return visitor.mark("lane_settlement")
}
func (visitor *exhaustiveVisitor) VisitObserverLossObserved(ObserverLossObserved) error {
	return visitor.mark("observer_loss")
}
func (visitor *exhaustiveVisitor) VisitReceiverTerminationObserved(ReceiverTerminationObserved) error {
	return visitor.mark("receiver_termination")
}

func TestVisitorDispatchCoversEverySealedVariant(t *testing.T) {
	failure, _ := appobs.NewFailure(appobs.FailureRelayTransport)
	authority, _ := appobs.NewRelayAuthority(appobs.RelayWSS, "relay.example", 443)
	subject, _ := appobs.NewDirectorySubject(appobs.NewDisplayName("photos"))
	subjectEvent, _ := NewSharingSubjectSelected(subject)
	relayConnected, _ := NewRelayConnected(CommandGet, authority)
	recovery, _ := NewRelayRecovering(CommandShare, authority, 1, appobs.RelayRecoveryStarted, appobs.Failure{})
	path, _ := NewContentPathSelected(appobs.ContentPathDirect)
	fallback, _ := NewFallback(CommandGet, appobs.TransportWebRTC, appobs.TransportRelay, failure)
	receiveID, _ := appobs.NewReceiveOperationID(bytes16(1))
	jobID, _ := appobs.NewTransferJobID(bytes16(2))
	snapshot, _ := appobs.NewProgressSnapshot(appobs.ProgressSpec{Discovery: appobs.DiscoveryOpen, CountersExact: true})
	progress, _ := NewTransferProgress(receiveID, jobID, snapshot)
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
	relayLifecycle, _ := NewRelayLifecycleObserved(appobs.RelayLifecycleSpec{
		Workflow: appobs.WorkflowReceive, LinkID: 1, SendOperationID: 1, Stage: appobs.RelayLinkClosed,
		RetirementSource: appobs.RelayRetirementLocalClose, Cause: appobs.RelayCauseNone, DrainCause: appobs.RelayCauseNone,
	})
	webRTCLifecycle, _ := NewWebRTCLifecycleObserved(appobs.WebRTCLifecycleSpec{
		Workflow: appobs.WorkflowReceive, ChannelID: 1, Operation: appobs.WebRTCChannel,
		Transition: appobs.WebRTCClosedClean, State: appobs.ChannelClosed,
		Terminal: appobs.WebRTCTerminalNone, Cause: appobs.WebRTCCauseNone,
	})
	peerPath, _ := appobs.NewPeerPathID(bytes16(4))
	peerAttemptID, _ := appobs.NewPeerAttemptID(bytes16(5))
	peerAttempt, _ := NewPeerAttemptObserved(appobs.PeerAttemptSpec{
		Workflow: appobs.WorkflowShare, Session: sessionID, PeerPath: peerPath,
		Attempt: peerAttemptID, Sequence: 1, Stage: appobs.PeerAttemptStarted,
	})
	transferLifecycle, _ := NewTransferLifecycleObserved(appobs.TransferLifecycleSpec{
		ReceiveOperation: receiveID, ProtocolSession: sessionID, TransferJob: jobID,
		Stage: appobs.TransferDiscoveryStarted, Progress: snapshot,
		FileSelection: appobs.FileSelectionNone, FileSettlement: appobs.FileSettlementNone,
		TreeSettlement: appobs.TreeSettlementNone,
	})
	filesystemOutput, _ := NewFilesystemOutputObserved(appobs.FilesystemOutputSpec{
		ReceiveOperation: receiveID, Operation: appobs.FilesystemRuntimeDecision,
	})
	senderTerminalSend, _ := NewSenderTerminalSendObserved(
		sessionID, lane, true, appobs.SenderTerminalSendAccepted,
		appobs.SenderTerminalSendDelivered, appobs.SenderTerminalSendDecisionDelivered,
	)
	senderSessionTerminated, _ := NewSenderSessionTerminated(
		sessionID, appobs.SenderSessionTerminalGracefulStop, appobs.SenderSessionTerminalNormalStop,
		diagnosticerror.Snapshot{},
	)
	catalogStorage, _ := NewCatalogStorageObserved(
		appobs.CatalogStorageRecovered, appobs.CatalogStorageCauseNone, appobs.CatalogUsage{}, 0,
	)
	rootPrefetch, _ := NewRootPrefetchObserved(appobs.RootPrefetchCommitted, 1, 2, 3)
	protocolOperationID, _ := appobs.NewProtocolOperationID(bytes16(6))
	protocolOperation, _ := NewProtocolOperationObserved(appobs.ProtocolOperationSpec{ObservedAt: time.Unix(1, 0),
		Workflow: appobs.WorkflowReceive, Role: appobs.ProtocolRoleReceiver,
		Stage:           appobs.ProtocolOperationReceiverFailed,
		ProtocolSession: sessionID, ProtocolOperation: protocolOperationID,
		RequestKind: appobs.ProtocolMessageReleaseLease,
		Lane:        lane, HasLane: true, Cause: appobs.ProtocolOperationCauseDeadline,
	})
	laneSettlement, _ := NewLaneSettlementObserved(appobs.LaneSettlementSpec{Session: sessionID, Route: appobs.LaneRouteDirect, Lane: lane})
	observerLoss, _ := NewObserverLossObserved(appobs.ObserverLossSpec{Workflow: appobs.WorkflowReceive, Category: appobs.ObserverLossCommandAdapter, Reason: appobs.ObserverLossEventContract, Count: 1})
	receiverTermination, _ := NewReceiverTerminationObserved(appobs.ReceiverTerminationSpec{
		LocalGeneration: 1, TransitionAuthority: appobs.ReceiverTerminalLocal,
		Disposition: appobs.ReceiverFallbackAllowed, TransitionProvenance: appobs.ReceiverProvenanceLocalExplicitStop,
		ConsequenceProvenance: appobs.ReceiverProvenanceLocalExplicitStop, LocalStopReason: appobs.ReceiverLocalStopCaller,
	})

	tests := []struct {
		name  string
		event Event
	}{
		{"ready", NewReady()},
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
	visitor := &exhaustiveVisitor{}
	for _, test := range tests {
		visitor.visited = ""
		if err := test.event.Accept(visitor); err != nil || visitor.visited != test.name {
			t.Fatalf("%T dispatched %q, want %q (err=%v)", test.event, visitor.visited, test.name, err)
		}
		if err := test.event.Accept(nil); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("%T accepted nil visitor: %v", test.event, err)
		}
	}
	if err := (RelayConnected{}).Accept(visitor); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero relay event dispatched: %v", err)
	}
	if err := (RootPrefetchObserved{}).Accept(visitor); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero root-prefetch event dispatched: %v", err)
	}
}

func TestRootPrefetchEventPreservesOnlyDecisionAndBoundedCounters(t *testing.T) {
	event, err := NewRootPrefetchObserved(appobs.RootPrefetchCommitted, 7, 11, 2)
	if err != nil {
		t.Fatal(err)
	}
	if event.Command() != CommandShare || event.Level() != appobs.LevelDebug ||
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
			if _, err := NewRootPrefetchObserved(test.decision, test.attempt, test.entries, test.omitted); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid root-prefetch event error = %v", err)
			}
		})
	}
	if _, err := NewRootPrefetchObserved(appobs.RootPrefetchStopped, 0, 0, 0); err != nil {
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
