package receiversession

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestReceiverPeerOperationLateTerminateCannotCrossSameIDGeneration(t *testing.T) {
	synctest.Test(t, testReceiverPeerOperationLateTerminateCannotCrossSameIDGeneration)
}

func testReceiverPeerOperationLateTerminateCannotCrossSameIDGeneration(t *testing.T) {
	now := time.Unix(15_000, 0)
	runtime, channel := newUnstartedRuntimeWithContinuations(t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2}, func() time.Time { return now }, continuationReplayClassifier{})
	var physicalSends atomic.Int32
	channel.onSend = func(framechannel.Frame) { physicalSends.Add(1) }
	startReceiverPeerWriter(t, runtime)
	open := func() (*PeerOperation, *sessioncore.Call, protocolsession.OperationGeneration) {
		rpc := newTestRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x86}, 8)))
		t.Cleanup(rpc.Close)
		receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
		operation, err := receiver.OpenPeerOperation(context.Background(), []byte{0xf6})
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := operation.call.OperationAuthority()
		return operation, operation.call, generation
	}
	operationA, callA, generationA := open()
	cancelPendingCall(t, operationA.rpc, callA)
	if generationA.IsActive() {
		t.Fatal("generation A retained authority after cancellation")
	}
	synctest.Wait()
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	operationB, callB, generationB := open()
	if generationA.Same(generationB) || !generationB.IsActive() || callA.ID() != callB.ID() {
		t.Fatal("fixture did not reuse the exact operation ID in a new generation")
	}
	// Route stale cleanup through the current request owner to exercise exact-handle rejection.
	operationA.rpc = operationB.rpc
	sendsBeforeLateCleanup := physicalSends.Load()
	terminationA := operationA.Terminate(context.Background())
	assertReceiverPeerTermination(t, operationA, terminationA,
		PeerTerminalAuthorityLocal, PeerProvenanceLocalExplicitStop, PeerTerminalOperationOnly, PeerProvenanceLocalExplicitStop, PeerDiagnosticCleanupFailed)
	if !generationB.IsActive() || physicalSends.Load() != sendsBeforeLateCleanup {
		t.Fatal("late generation A cleanup changed B or emitted a stale cancellation")
	}
	if _, err := operationB.SendCandidate(context.Background(), []byte{0xf6}); err != nil {
		t.Fatalf("generation B continuation: %v", err)
	}
	assertReceiverPeerTermination(t, operationB, operationB.Terminate(context.Background()),
		PeerTerminalAuthorityLocal, PeerProvenanceLocalExplicitStop, PeerTerminalOperationOnly, PeerProvenanceLocalExplicitStop)
	if generationB.IsActive() {
		t.Fatal("generation B retained authority after termination")
	}
}

func issuedOperationID(prefix byte, counter uint64) protocolsession.OperationID {
	return issuedOperationIDWithPrefix([8]byte{
		prefix, prefix, prefix, prefix, prefix, prefix, prefix, prefix,
	}, counter)
}

func issuedOperationIDWithPrefix(prefix [8]byte, counter uint64) protocolsession.OperationID {
	var identity protocolsession.OperationID
	copy(identity[:8], prefix[:])
	binary.BigEndian.PutUint64(identity[8:], counter)
	return identity
}
