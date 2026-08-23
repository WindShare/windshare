package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func TestAuthenticatedCapacitySignalPreservesExactHintAndCorrelation(t *testing.T) {
	sessionID := id16[protocolsession.ProtocolSessionID](181)
	operationID := id16[protocolsession.OperationID](182)
	generation, err := revisionwait.GenerationTokenFromBytes(sessionID.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	dependencies := receiverTransferDependencies{
		runtime:    &ReceiverRuntime{runtimeCore: &runtimeCore{sessionID: sessionID}},
		generation: generation,
	}
	remote := &RemoteRevisionError{
		failure: contentflow.RevisionFailure{
			Code: contentflow.RevisionCodeQuota, Retryable: true, RetryAfter: 275 * time.Millisecond,
		},
		protocolSession: sessionID, protocolOperation: operationID,
	}
	capacity, recognized := dependencies.authenticatedCapacitySignal(remote)
	if !recognized {
		t.Fatal("authenticated capacity result was not recognized")
	}
	signal, ok := revisionwait.MatchCapacitySignal(capacity)
	if !ok || signal.RetryAfter() != remote.Failure().RetryAfter ||
		!signal.ProtocolSession().Equal(sessionID) || signal.ProtocolOperation() != operationID ||
		signal.Generation() != generation {
		t.Fatalf("capacity signal=%+v matched=%v", signal, ok)
	}
	if remote.ProtocolSessionID() != sessionID || remote.OperationID() != operationID {
		t.Fatalf("remote correlation session=%x operation=%x", remote.ProtocolSessionID(), remote.OperationID())
	}
}

func TestAuthenticatedOpenCapacityResponsePreservesWireCorrelation(t *testing.T) {
	fixture := newVerticalFixture(t)
	fixture.contentStore.openErr = capacityBusyForAdapterTest(t, 125*time.Millisecond)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	_, failure := receiver.OpenRevision(context.Background(), fixture.fileID)
	var remote *RemoteRevisionError
	if !errors.As(failure, &remote) || remote == nil {
		t.Fatalf("authenticated OPEN failure lost remote item diagnostic: %T %v", failure, failure)
	}
	if remote.Failure().Code != contentflow.RevisionCodeQuota || !remote.Failure().Retryable ||
		remote.Failure().RetryAfter != 125*time.Millisecond ||
		!remote.ProtocolSessionID().Equal(receiver.ProtocolSessionID()) || remote.OperationID().IsZero() {
		t.Fatalf("authenticated capacity diagnostic failure=%+v session=%x operation=%x",
			remote.Failure(), remote.ProtocolSessionID(), remote.OperationID())
	}
	generation, err := revisionwait.GenerationTokenFromBytes(receiver.ProtocolSessionID().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	capacity, recognized := (receiverTransferDependencies{
		runtime: receiver, generation: generation,
	}).authenticatedCapacitySignal(remote)
	signal, matched := revisionwait.MatchCapacitySignal(capacity)
	if !recognized || !matched || signal.RetryAfter() != 125*time.Millisecond ||
		signal.ProtocolOperation() != remote.OperationID() {
		t.Fatalf("wire capacity signal=%+v recognized=%v matched=%v", signal, recognized, matched)
	}
}

func TestAuthenticatedCapacitySignalRejectsAdjacentFailureShapes(t *testing.T) {
	sessionID := id16[protocolsession.ProtocolSessionID](183)
	operationID := id16[protocolsession.OperationID](184)
	generation, _ := revisionwait.GenerationTokenFromBytes(sessionID.Bytes())
	dependencies := receiverTransferDependencies{
		runtime:    &ReceiverRuntime{runtimeCore: &runtimeCore{sessionID: sessionID}},
		generation: generation,
	}
	for name, failure := range map[string]contentflow.RevisionFailure{
		"permanent quota": {
			Code: contentflow.RevisionCodeQuota,
		},
		"retryable non-capacity": {
			Code: contentflow.RevisionCodeUnreadable, Retryable: true, RetryAfter: time.Millisecond,
		},
		"missing retry hint": {
			Code: contentflow.RevisionCodeQuota, Retryable: true,
		},
	} {
		remote := &RemoteRevisionError{
			failure: failure, protocolSession: sessionID, protocolOperation: operationID,
		}
		if capacity, recognized := dependencies.authenticatedCapacitySignal(remote); recognized || capacity != nil {
			t.Errorf("%s acquired capacity authority: %T %v", name, capacity, capacity)
		}
	}

	foreign := &RemoteRevisionError{
		failure: contentflow.RevisionFailure{
			Code: contentflow.RevisionCodeQuota, Retryable: true, RetryAfter: time.Millisecond,
		},
		protocolSession: id16[protocolsession.ProtocolSessionID](185), protocolOperation: operationID,
	}
	capacity, recognized := dependencies.authenticatedCapacitySignal(foreign)
	if !recognized {
		t.Fatal("capacity tuple with invalid correlation was silently treated as a generic failure")
	}
	if _, ok := revisionwait.MatchCapacitySignal(capacity); ok {
		t.Fatal("foreign protocol correlation produced retry authority")
	}
}

func TestReceiverRevisionWaitFenceEndsWithFixedRuntime(t *testing.T) {
	sessionID := id16[protocolsession.ProtocolSessionID](186)
	lifecycle, endRuntime := context.WithCancel(context.Background())
	runtime := &ReceiverRuntime{runtimeCore: &runtimeCore{sessionID: sessionID, ctx: lifecycle}}
	generation, config, err := newReceiverRevisionWait(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.GenerationFence == nil || config.GenerationFence.Current() != generation {
		t.Fatalf("revision wait config=%+v generation=%x", config, generation)
	}
	type fenceResult struct {
		change revisionwait.GenerationChange
		err    error
	}
	result := make(chan fenceResult, 1)
	go func() {
		change, waitErr := config.GenerationFence.WaitForChange(context.Background(), generation)
		result <- fenceResult{change: change, err: waitErr}
	}()
	endRuntime()
	select {
	case ended := <-result:
		if ended.err != nil || ended.change.Kind() != revisionwait.GenerationLifetimeEnded ||
			ended.change.Previous() != generation || !ended.change.Current().IsZero() || ended.change.Cause() == nil {
			t.Fatalf("runtime fence result=%+v err=%v", ended.change, ended.err)
		}
	case <-time.After(time.Second):
		t.Fatal("fixed runtime end did not wake capacity wait fence")
	}
	if _, _, err := newReceiverRevisionWait(nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil runtime wait config error=%v", err)
	}
}

func capacityBusyForAdapterTest(t *testing.T, retryAfter time.Duration) *revisioncapacity.CapacityBusyError {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{
		Limits:     revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 8},
		RetryAfter: retryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := owner.Coordinator().RegisterStore(revisioncapacity.StoreConfig{
		StoreID: "sessionruntime-adapter-store", ShareID: "sessionruntime-adapter-share",
		Limits: revisioncapacity.CapacityLimits{StableHandles: 8, ActiveLeases: 8},
	}, verticalCapacityTarget{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: "sessionruntime-adapter-session",
		Limits:    revisioncapacity.CapacityLimits{StableHandles: 8, ActiveLeases: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Admit(context.Background(), revisioncapacity.AdmissionRequest{
		Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "resident", Session: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	charges, err := grant.Commit()
	if err != nil {
		t.Fatal(err)
	}
	_, busyErr := store.Admit(context.Background(), revisioncapacity.AdmissionRequest{
		Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "blocked", Session: session,
	})
	var busy *revisioncapacity.CapacityBusyError
	if !errors.As(busyErr, &busy) {
		t.Fatalf("capacity admission error=%T %v", busyErr, busyErr)
	}
	if err := charges.Release(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	return busy
}
