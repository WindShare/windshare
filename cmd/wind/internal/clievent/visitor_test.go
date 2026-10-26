package clievent

import (
	"errors"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
)

func TestApplicationFactsAndCommandResultsValidate(t *testing.T) {
	failure, _ := appobs.NewFailure(appobs.FailureRelayTransport)
	origin, _ := appobs.NewRelayOrigin(appobs.RelayWSS, "relay.example", 443)
	subject, _ := appobs.NewDirectorySubject(appobs.NewDisplayName("photos"))
	subjectEvent, _ := appobs.NewSharingSubjectSelected(subject)
	relayConnected, _ := NewRelayConnected(CommandGet, origin)
	recovery, _ := NewRelayRecovering(CommandShare, origin, 1, appobs.RelayRecoveryStarted, appobs.Failure{})
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
	observerLoss, _ := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{Workflow: appobs.WorkflowReceive, Category: appobs.ObserverLossCommandAdapter, Reason: appobs.ObserverLossEventContract, Count: 1})

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
		{"observer_loss", observerLoss},
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

}

func bytes16(seed byte) []byte {
	value := make([]byte, appobs.IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}
