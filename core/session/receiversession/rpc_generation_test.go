package receiversession

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverPeerOperationLateTerminateCannotAffectSubsequentOperation(t *testing.T) {
	synctest.Test(t, testReceiverPeerOperationLateTerminateCannotAffectSubsequentOperation)
}

func testReceiverPeerOperationLateTerminateCannotAffectSubsequentOperation(t *testing.T) {
	runtime, channel := newUnstartedRuntimeWithContinuations(t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 1}, nil, continuationReplayClassifier{})
	var physicalSends atomic.Int32
	channel.onSend = func(framechannel.Frame) { physicalSends.Add(1) }
	startReceiverPeerWriter(t, runtime)
	rpc := newTestRequests(runtime, nil)
	t.Cleanup(rpc.Close)
	receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
	open := func() (*PeerOperation, *sessioncore.Call, protocolsession.Operation) {
		operation, err := receiver.OpenPeerOperation(context.Background(), []byte{0xf6})
		if err != nil {
			t.Fatal(err)
		}
		return operation, operation.call, operation.call.Operation()
	}
	operationA, callA, opA := open()
	cancelPendingCall(t, operationA.rpc, callA)
	if opA.IsActive() {
		t.Fatal("operation A retained authority after cancellation")
	}
	synctest.Wait()
	operationB, callB, opB := open()
	if opA.Same(opB) || !opB.IsActive() || callA.ID() == callB.ID() {
		t.Fatal("monotonic IDs collided or operation B did not become active")
	}
	sendsBeforeLateCleanup := physicalSends.Load()
	terminationA := operationA.Terminate(context.Background())
	assertReceiverPeerTermination(t, operationA, terminationA,
		PeerTerminalAuthorityLocal, PeerProvenanceLocalExplicitStop, PeerTerminalOperationOnly, PeerProvenanceLocalExplicitStop)
	if !opB.IsActive() || physicalSends.Load() != sendsBeforeLateCleanup {
		t.Fatal("late operation A cleanup changed B or emitted a stale cancellation")
	}
	if _, err := operationB.SendCandidate(context.Background(), []byte{0xf6}); err != nil {
		t.Fatalf("operation B continuation: %v", err)
	}
	assertReceiverPeerTermination(t, operationB, operationB.Terminate(context.Background()),
		PeerTerminalAuthorityLocal, PeerProvenanceLocalExplicitStop, PeerTerminalOperationOnly, PeerProvenanceLocalExplicitStop)
	if opB.IsActive() {
		t.Fatal("operation B retained authority after termination")
	}
}
