package clievent

import (
	"errors"
	"testing"
)

type exhaustiveVisitor struct{ visited string }

func (visitor *exhaustiveVisitor) mark(name string) error { visitor.visited = name; return nil }
func (visitor *exhaustiveVisitor) VisitReady(Ready) error { return visitor.mark("ready") }
func (visitor *exhaustiveVisitor) VisitSharingSubjectSelected(SharingSubjectSelected) error {
	return visitor.mark("sharing_subject")
}
func (visitor *exhaustiveVisitor) VisitRelayConnected(RelayConnected) error {
	return visitor.mark("relay_connected")
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
func (visitor *exhaustiveVisitor) VisitSenderTerminalObserved(SenderTerminalObserved) error {
	return visitor.mark("sender_terminal")
}
func (visitor *exhaustiveVisitor) VisitCatalogStorageObserved(CatalogStorageObserved) error {
	return visitor.mark("catalog_storage")
}
func (visitor *exhaustiveVisitor) VisitRootPrefetchObserved(RootPrefetchObserved) error {
	return visitor.mark("root_prefetch")
}
func (visitor *exhaustiveVisitor) VisitProtocolOperationObserved(ProtocolOperationObserved) error {
	return visitor.mark("protocol_operation")
}

func TestVisitorDispatchCoversEverySealedVariant(t *testing.T) {
	failure, _ := NewFailure(FailureRelayTransport)
	authority, _ := NewRelayAuthority(RelayWSS, "relay.example", 443)
	subject, _ := NewDirectorySubject(NewDisplayName("photos"))
	subjectEvent, _ := NewSharingSubjectSelected(subject)
	relayConnected, _ := NewRelayConnected(CommandGet, authority)
	recovery, _ := NewRelayRecovering(CommandShare, authority, 1, RelayRecoveryStarted, Failure{})
	path, _ := NewContentPathSelected(ContentPathDirect)
	fallback, _ := NewFallback(CommandGet, TransportWebRTC, TransportRelay, failure)
	receiveID, _ := NewReceiveOperationID(bytes16(1))
	jobID, _ := NewTransferJobID(bytes16(2))
	snapshot, _ := NewProgressSnapshot(ProgressSpec{Discovery: DiscoveryOpen, CountersExact: true})
	progress, _ := NewTransferProgress(receiveID, jobID, snapshot)
	warning, _ := NewWarning(CommandGet, failure)
	failed, _ := NewCommandFailed(CommandGet, ExitNetwork, failure)
	transferResult, _ := NewTransferResult(TransferResultSpec{
		Status: ResultSuccess, ExitCode: ExitSuccess, Drift: DriftNone,
		Destination: NewDisplayPath("result"), CountersExact: true,
	})
	settled, _ := NewTransferSettled(transferResult)
	shareResult, _ := NewShareResult(ShareResultSpec{ExitCode: ExitSuccess})
	stopped, _ := NewSharingStopped(shareResult)
	incomplete, _ := NewTraceIncomplete(CommandGet, TraceIncompleteWriter, 0, 0)
	sessionID, _ := NewProtocolSessionID(bytes16(3))
	lane, _ := NewLaneIdentity(2, 1)
	adopted, _ := NewLaneAdopted(CommandGet, sessionID, lane, TransportWebRTC)
	relayLifecycle, _ := NewRelayLifecycleObserved(RelayLifecycleSpec{
		Command: CommandGet, LinkID: 1, Stage: RelayLinkClosed,
		RetirementSource: RelayRetirementNone, Cause: RelayCauseNone, DrainCause: RelayCauseNone,
	})
	webRTCLifecycle, _ := NewWebRTCLifecycleObserved(WebRTCLifecycleSpec{
		Command: CommandGet, ChannelID: 1, Operation: WebRTCChannel,
		Transition: WebRTCClosedClean, State: ChannelClosed,
		Terminal: WebRTCTerminalNone, Cause: WebRTCCauseNone,
	})
	peerPath, _ := NewPeerPathID(bytes16(4))
	peerAttemptID, _ := NewPeerAttemptID(bytes16(5))
	peerAttempt, _ := NewPeerAttemptObserved(PeerAttemptSpec{
		Command: CommandShare, Session: sessionID, PeerPath: peerPath,
		Attempt: peerAttemptID, Sequence: 1, Stage: PeerAttemptStarted,
	})
	transferLifecycle, _ := NewTransferLifecycleObserved(TransferLifecycleSpec{
		ReceiveOperation: receiveID, ProtocolSession: sessionID, TransferJob: jobID,
		Stage: TransferDiscoveryStarted, Progress: snapshot,
		FileSelection: FileSelectionNone, FileSettlement: FileSettlementNone,
		TreeSettlement: TreeSettlementNone,
	})
	filesystemOutput, _ := NewFilesystemOutputObserved(
		receiveID, FilesystemRuntimeDecision, FilesystemOutputCounters{}, Failure{},
	)
	senderTerminal, _ := NewSenderTerminalObserved(
		sessionID, lane, true, SenderTerminalAccepted,
		SenderTerminalDelivered, SenderTerminalDecisionDelivered,
	)
	catalogStorage, _ := NewCatalogStorageObserved(
		CatalogStorageRecovered, CatalogStorageCauseNone, CatalogUsage{}, 0,
	)
	rootPrefetch, _ := NewRootPrefetchObserved(RootPrefetchCommitted, 1, 2, 3)
	protocolOperationID, _ := NewProtocolOperationID(bytes16(6))
	protocolOperation, _ := NewProtocolOperationObserved(ProtocolOperationSpec{
		Command: CommandGet, Role: ProtocolRoleReceiver,
		Stage:           ProtocolOperationReceiverFailed,
		ProtocolSession: sessionID, ProtocolOperation: protocolOperationID,
		RequestKind: ProtocolMessageReleaseLease,
		Lane:        lane, HasLane: true, Cause: ProtocolOperationCauseDeadline,
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
		{"sender_terminal", senderTerminal},
		{"catalog_storage", catalogStorage},
		{"root_prefetch", rootPrefetch},
		{"protocol_operation", protocolOperation},
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
	event, err := NewRootPrefetchObserved(RootPrefetchCommitted, 7, 11, 2)
	if err != nil {
		t.Fatal(err)
	}
	if event.Command() != CommandShare || event.Level() != LevelDebug ||
		event.Decision() != RootPrefetchCommitted || event.Attempt() != 7 ||
		event.EntryCount() != 11 || event.OmittedCount() != 2 {
		t.Fatalf("root-prefetch event = %#v", event)
	}
	for _, test := range []struct {
		name     string
		decision RootPrefetchDecision
		attempt  uint64
		entries  uint64
		omitted  uint64
	}{
		{"unknown decision", RootPrefetchDecision(255), 1, 0, 0},
		{"missing attempt", RootPrefetchAttemptStarted, 0, 0, 0},
		{"non-commit entries", RootPrefetchScanFailed, 1, 1, 0},
		{"non-commit omissions", RootPrefetchStopped, 0, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRootPrefetchObserved(test.decision, test.attempt, test.entries, test.omitted); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid root-prefetch event error = %v", err)
			}
		})
	}
	if _, err := NewRootPrefetchObserved(RootPrefetchStopped, 0, 0, 0); err != nil {
		t.Fatalf("pre-attempt stop rejected: %v", err)
	}
}

func bytes16(seed byte) []byte {
	value := make([]byte, IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}
