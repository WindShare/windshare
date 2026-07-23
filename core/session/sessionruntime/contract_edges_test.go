package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type receiverResourceSourceFunc func() (ReceiverRuntimeResourceLease, error)

func (source receiverResourceSourceFunc) AcquireReceiverRuntimeResources() (
	ReceiverRuntimeResourceLease,
	error,
) {
	return source()
}

type countedReceiverResourceLease struct{ releases atomic.Int32 }

func (lease *countedReceiverResourceLease) Release() { lease.releases.Add(1) }

type failIdentityReadReader struct{ failures atomic.Int32 }

func (reader *failIdentityReadReader) Read(buffer []byte) (int, error) {
	if len(buffer) == protocolsession.IdentityBytes {
		reader.failures.Add(1)
		return 0, io.ErrUnexpectedEOF
	}
	for index := range buffer {
		buffer[index] = byte(index + 1)
	}
	return len(buffer), nil
}

func TestReceiverRuntimeResourceLeaseCoversFailureAndSuccessLifecycles(t *testing.T) {
	fixture := newVerticalFixture(t)
	wantAcquireErr := errors.New("receiver resources closed")
	for name, source := range map[string]ReceiverRuntimeResourceSource{
		"acquire failure": receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
			return nil, wantAcquireErr
		}),
		"nil lease": receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
			return nil, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			factory := &ReceiverFactory{resources: source}
			_, err := factory.acquireRuntimeResources()
			if name == "acquire failure" && !errors.Is(err, wantAcquireErr) {
				t.Fatalf("resource acquisition error = %v", err)
			}
			if name == "nil lease" && !errors.Is(err, ErrRuntimeConfig) {
				t.Fatalf("nil resource lease error = %v", err)
			}
		})
	}
	connectSourceCalls := atomic.Int32{}
	connectConfig := fixture.receiverConfig
	connectConfig.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		connectSourceCalls.Add(1)
		return nil, wantAcquireErr
	})
	connectFactory, err := NewReceiverFactory(connectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connectFactory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, wantAcquireErr) {
		t.Fatalf("Connect resource acquisition error = %v", err)
	}
	if connectSourceCalls.Load() != 1 {
		t.Fatalf("Connect acquired resources %d times", connectSourceCalls.Load())
	}

	failedLease := &countedReceiverResourceLease{}
	failureConfig := fixture.receiverConfig
	failureConfig.Random = edgeErrorReader{}
	failureConfig.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		return failedLease, nil
	})
	failureFactory, err := NewReceiverFactory(failureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failureFactory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrHandshake) {
		t.Fatalf("handshake failure with resource lease = %v", err)
	}
	if failedLease.releases.Load() != 1 {
		t.Fatalf("failed connection released resources %d times", failedLease.releases.Load())
	}

	activeLease := &countedReceiverResourceLease{}
	successConfig := fixture.receiverConfig
	successConfig.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		return activeLease, nil
	})
	successFactory, err := NewReceiverFactory(successConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, successFactory)
	if activeLease.releases.Load() != 0 {
		t.Fatal("connected runtime released resources before Close")
	}
	receiver.Close()
	receiver.Close()
	if activeLease.releases.Load() != 1 {
		t.Fatalf("connected runtime released resources %d times", activeLease.releases.Load())
	}
	sender.Close()
}

func TestRuntimeDonePublishesReceiverAndSenderResourceCleanup(t *testing.T) {
	fixture := newVerticalFixture(t)
	receiverLease := &countedReceiverResourceLease{}
	receiverConfig := fixture.receiverConfig
	receiverConfig.RuntimeResources = receiverResourceSourceFunc(func() (ReceiverRuntimeResourceLease, error) {
		return receiverLease, nil
	})
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan content.LeaseID, 1)
	fixture.contentStore.released = released
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); err != nil {
		t.Fatal(err)
	}
	if err := sender.Stop(context.Background(), "test terminal"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-receiver.Done():
	case <-time.After(time.Second):
		t.Fatal("receiver did not publish terminal cleanup")
	}
	if receiverLease.releases.Load() != 1 {
		t.Fatalf("receiver resource releases at Done=%d", receiverLease.releases.Load())
	}
	select {
	case leaseID := <-released:
		if leaseID != fixture.contentStore.lease.ID() {
			t.Fatalf("sender released lease=%x", leaseID)
		}
	default:
		t.Fatal("sender Done retained its per-session content lease")
	}
	receiver.rpc.mu.Lock()
	rpcClosed, calls := receiver.rpc.closed, len(receiver.rpc.calls)
	receiver.rpc.mu.Unlock()
	receiver.revisions.mu.Lock()
	revisionsClosed, leases := receiver.revisions.closed, len(receiver.revisions.leases)
	receiver.revisions.mu.Unlock()
	if !rpcClosed || calls != 0 || !revisionsClosed || leases != 0 {
		t.Fatalf("receiver Done state rpcClosed=%v calls=%d revisionsClosed=%v leases=%d",
			rpcClosed, calls, revisionsClosed, leases)
	}
}

func TestReceiverConnectPropagatesDefaultIdentityEntropyAndHandshakeCancellation(t *testing.T) {
	t.Run("default receiver identity entropy", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		random := &failIdentityReadReader{}
		config := fixture.receiverConfig
		config.Random = random
		config.ReceiverInstances = nil
		factory, err := NewReceiverFactory(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := factory.Connect(ctx, newMemoryChannel(t)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("default receiver identity entropy error = %v", err)
		}
		if random.failures.Load() != 1 {
			t.Fatalf("receiver identity entropy failures = %d, want 1", random.failures.Load())
		}
	})

	t.Run("handshake wait cancellation", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		receiverChannel, peerChannel := newMemoryChannelPair()
		t.Cleanup(func() {
			_ = receiverChannel.Close()
			_ = peerChannel.Close()
		})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := fixture.receiverFactory.Connect(ctx, receiverChannel)
			result <- err
		}()
		select {
		case <-peerChannel.Recv():
			cancel()
		case <-time.After(time.Second):
			t.Fatal("receiver did not send ClientHello")
		}
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake cancellation error = %v", err)
		}
	})
}

func TestRegisterSenderHandlersPropagatesEveryOwnershipConflict(t *testing.T) {
	duplicateKinds := []protocolsession.MessageKind{
		protocolsession.MessageListChildren,
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageLaneAttach,
		protocolsession.MessagePeerOffer,
		protocolsession.MessageCancel,
	}
	for _, duplicate := range duplicateKinds {
		t.Run(fmt.Sprintf("kind-%d", duplicate), func(t *testing.T) {
			operations, err := protocolsession.NewOperationTable(
				protocolsession.OperationLimits{MaxActive: 16, MaxTombstones: 16}, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			router, err := protocolsession.NewRoleRouter(protocolsession.RoleSender, operations)
			if err != nil {
				t.Fatal(err)
			}
			if err := router.RegisterHandler(duplicate, protocolsession.MessageHandlerFunc(
				func(context.Context, protocolsession.Message) error { return nil },
			)); err != nil {
				t.Fatal(err)
			}
			err = registerSenderHandlers(
				router,
				&catalogHandler{},
				&contentflow.SenderHandler{},
				&laneGrantHandler{},
				inertSenderPeerHandler{},
			)
			if !errors.Is(err, protocolsession.ErrHandlerRegistered) {
				t.Fatalf("duplicate handler %d error = %v", duplicate, err)
			}
		})
	}
}

func TestCatalogHandlerSuppressesDuplicateWorkAndCancelsQueuedOperations(t *testing.T) {
	handler := newCatalogHandler(nil, senderOutbound{})
	for range cap(handler.workers) {
		handler.workers <- struct{}{}
	}
	duplicateID := id16[protocolsession.OperationID](61)
	queuedID := id16[protocolsession.OperationID](62)
	barrierID := id16[protocolsession.OperationID](63)
	listMessage := func(operationID protocolsession.OperationID) protocolsession.Message {
		message, err := protocolsession.NewMessage(
			protocolsession.MessageListChildren, &operationID, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	duplicateMessage := listMessage(duplicateID)
	duplicateContext := senderIngressContext(t, duplicateMessage)
	duplicateKey, _ := catalogKey(duplicateContext, duplicateID)
	if !handler.add(duplicateKey, func() {}) {
		t.Fatal("failed to seed active catalog operation")
	}
	queuedMessage := listMessage(queuedID)
	queuedContext := senderIngressContext(t, queuedMessage)
	barrierMessage := listMessage(barrierID)
	barrierContext := senderIngressContext(t, barrierMessage)
	barrierKey, _ := catalogKey(barrierContext, barrierID)
	barrierReached := make(chan struct{})
	var barrierOnce sync.Once
	if !handler.add(barrierKey, func() { barrierOnce.Do(func() { close(barrierReached) }) }) {
		t.Fatal("failed to seed catalog barrier")
	}
	for _, queued := range []struct {
		ctx     context.Context
		message protocolsession.Message
	}{
		{duplicateContext, duplicateMessage},
		{queuedContext, queuedMessage},
		{queuedContext, operationMessageForTest(t, protocolsession.MessageCancel, queuedID, validCancelBody(t))},
		{barrierContext, operationMessageForTest(t, protocolsession.MessageCancel, barrierID, validCancelBody(t))},
	} {
		if err := handler.HandleMessage(queued.ctx, queued.message); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- handler.Run(ctx) }()
	select {
	case <-barrierReached:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("catalog handler did not reach the FIFO cancellation barrier")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("catalog handler cancellation error = %v", err)
	}
	if err := handler.progressObserver(context.Background(), queuedID).ObserveScanProgress(
		context.Background(), catalog.ScanProgress{},
	); !errors.Is(err, protocolsession.ErrInvalidScanProgress) {
		t.Fatalf("zero scan attempt error = %v", err)
	}
}

func TestSenderPeerSessionRejectsMissingAuthorityAndExposesExactIdentity(t *testing.T) {
	var empty senderPeerSession
	if !empty.ShareInstance().IsZero() || !empty.ProtocolSessionID().IsZero() {
		t.Fatal("empty peer authority exposed a nonzero identity")
	}
	if _, err := empty.SendPeerControl(
		context.Background(), protocolsession.MessagePeerAnswer,
		protocolsession.OperationID{}, nil,
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("empty peer control error = %v", err)
	}
	if _, err := empty.AdmitPeerChannel(context.Background(), nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("empty peer admission error = %v", err)
	}
	if err := empty.FailPeerOperation(
		context.Background(), protocolsession.OperationID{},
		protocolsession.PeerOperationCodeNegotiation, "missing",
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("empty peer failure error = %v", err)
	}
	if _, err := (SenderPeerHandlerFactoryFunc(nil)).NewSenderPeerHandler(empty); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil peer handler factory error = %v", err)
	}

	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	authority := senderPeerSession{runtime: sender}
	if authority.ShareInstance() != fixture.share || authority.ProtocolSessionID() != sender.ProtocolSessionID() {
		t.Fatal("peer authority changed its ProtocolSession identity")
	}
}

func TestReceiverPeerOperationRejectsMalformedAndCrossScopeFinals(t *testing.T) {
	if _, err := (*ReceiverRuntime)(nil).OpenPeerOperation(context.Background(), nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil receiver peer operation error = %v", err)
	}
	var nilOperation *ReceiverPeerOperation
	if !nilOperation.OperationID().IsZero() {
		t.Fatal("nil peer operation exposed an identity")
	}
	if _, err := nilOperation.SendCandidate(context.Background(), nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil peer candidate error = %v", err)
	}
	nilReceive := nilOperation.Receive(context.Background())
	if control, ok := nilReceive.Control(); ok {
		t.Fatalf("nil peer receive exposed control = %+v", control)
	}
	if termination, ok := nilReceive.Termination(); ok {
		t.Fatalf("nil peer receive manufactured termination = %+v", termination)
	}
	nilTermination := nilOperation.Terminate(context.Background())
	if nilTermination.Authority() != receiverPeerTerminalAuthorityInvalid ||
		nilTermination.Severity() != receiverPeerTerminalSeverityInvalid {
		t.Fatalf("nil peer termination = %+v", nilTermination)
	}

	fixture := newVerticalFixture(t)
	receiverConfig := fixture.receiverConfig
	receiverConfig.PeerControls = receiverPeerSemanticsForTest(
		protocolsession.SenderControlSemanticValidatorFunc(func(
			protocolsession.MessageKind,
			protocolsession.OperationID,
			[]byte,
		) error {
			return nil
		}),
	)
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	t.Run("unexpected response kind", func(t *testing.T) {
		operation := openTestPeerOperation(t, receiver)
		message, err := protocolsession.NewMessage(
			protocolsession.MessageCatalogResult, &operation.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(operation.call, message)
		termination := requireReceiverPeerTermination(t, operation.Receive(context.Background()))
		assertReceiverPeerTermination(
			t,
			operation,
			termination,
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerTerminalOperationOnly,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerDiagnosticUnknownControl,
		)
		if receiverPeerDiagnosticsContain(termination.Diagnostics(), ReceiverPeerDiagnosticOperationMissing) {
			t.Fatalf("unexpected response retained cleanup wakeup diagnostic: %+v", termination.Diagnostics().Components())
		}
	})

	t.Run("cross-scope operation error", func(t *testing.T) {
		operation := openTestPeerOperation(t, receiver)
		semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
			Scope:   protocolsession.OperationScopeBlock,
			Code:    contentflow.BlockCodeInvalidRef,
			Message: "block failure on peer operation",
		})
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(operation.call, signedPeerOperationControl(
			t, receiver, fixture.senderFactory.privateKey,
			protocolsession.MessageOperationError, operation.call.id, semantic,
		))
		termination := requireReceiverPeerTermination(t, operation.Receive(context.Background()))
		assertReceiverPeerTermination(
			t,
			operation,
			termination,
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteFailureScopeViolation,
			ReceiverPeerTerminalSessionUnsafe,
			ReceiverPeerProvenanceRemoteFailureScopeViolation,
			ReceiverPeerDiagnosticRemoteFailureScopeViolation,
		)
		failure := requireReceiverPeerRemoteFailure(
			t,
			termination,
			ReceiverPeerDiagnosticRemoteFailureScopeViolation,
		)
		if failure.Scope() != protocolsession.OperationScopeBlock {
			t.Fatalf("cross-scope peer failure snapshot = %+v", failure)
		}
	})

	t.Run("cancelled receive", func(t *testing.T) {
		operation := openTestPeerOperation(t, receiver)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		termination := requireReceiverPeerTermination(t, operation.Receive(ctx))
		assertReceiverPeerTermination(
			t,
			operation,
			termination,
			ReceiverPeerTerminalAuthorityLocal,
			ReceiverPeerProvenanceLocalContextEnded,
			ReceiverPeerTerminalOperationOnly,
			ReceiverPeerProvenanceLocalContextEnded,
			ReceiverPeerDiagnosticContextCanceled,
		)
	})

	t.Run("cancel after runtime close finalizes locally", func(t *testing.T) {
		operation := openTestPeerOperation(t, receiver)
		operationID := operation.OperationID()
		receiver.Close()
		termination := operation.Terminate(context.Background())
		assertReceiverPeerRuntimeTermination(t, operation, termination)
		if operation.OperationID() != operationID {
			t.Fatal("terminal peer operation changed its stable identity")
		}
	})
}

func TestMalformedPeerResponseCancelsExactGenerationAndDrainsSenderAttempt(t *testing.T) {
	for _, malformedKind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerAnswer,
		protocolsession.MessagePeerCandidate,
	} {
		t.Run(fmt.Sprintf("kind-%d", malformedKind), func(t *testing.T) {
			fixture := newVerticalFixture(t)
			validAnswer, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "valid-answer"})
			if err != nil {
				t.Fatal(err)
			}
			created := make(chan *trackedSenderPeerHandler, 1)
			fixture.senderFactory.peers = SenderPeerHandlerFactoryFunc(func(
				session SenderPeerSession,
			) (SenderPeerHandler, error) {
				handler := newTrackedSenderPeerHandler(session, validAnswer)
				created <- handler
				return handler, nil
			})
			receiverConfig := fixture.receiverConfig
			receiverConfig.PeerControls = receiverPeerSemanticsForTest(protocolsession.SenderControlSemanticValidatorFunc(func(
				kind protocolsession.MessageKind,
				_ protocolsession.OperationID,
				semantic []byte,
			) error {
				if kind == protocolsession.MessagePeerAnswer && bytes.Equal(semantic, validAnswer) {
					return nil
				}
				return protocolsession.ErrControlSemantic
			}))
			receiverFactory, err := NewReceiverFactory(receiverConfig)
			if err != nil {
				t.Fatal(err)
			}
			sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
			defer sender.Close()
			defer receiver.Close()
			handler := <-created

			malformed := openTestPeerOperation(t, receiver)
			malformedID := malformed.OperationID()
			if malformedID.IsZero() {
				t.Fatal("malformed peer attempt exposed no operation identity before cleanup")
			}
			if observed := waitPeerOperationID(t, handler.offers, "malformed offer"); observed != malformedID {
				t.Fatalf("sender observed malformed offer ID=%x, want %x", observed, malformedID)
			}
			malformedCall := malformed.call
			malformedGeneration, _ := malformedCall.operationAuthority()
			if malformedGeneration.IsZero() || !malformedGeneration.IsActive() {
				t.Fatal("malformed peer attempt has no active generation")
			}

			sibling := openTestPeerOperation(t, receiver)
			siblingID := sibling.OperationID()
			if siblingID.IsZero() || siblingID == malformedID {
				t.Fatalf("sibling peer identity malformed=%x sibling=%x", malformedID, siblingID)
			}
			if observed := waitPeerOperationID(t, handler.offers, "sibling offer"); observed != siblingID {
				t.Fatalf("sender observed sibling offer ID=%x, want %x", observed, siblingID)
			}
			siblingGeneration, _ := sibling.call.operationAuthority()

			message, err := protocolsession.NewMessage(malformedKind, &malformedID, []byte{1})
			if err != nil {
				t.Fatal(err)
			}
			enqueueCallResponse(malformedCall, message)
			malformedTermination := requireReceiverPeerTermination(
				t,
				malformed.Receive(context.Background()),
			)
			assertReceiverPeerTermination(
				t,
				malformed,
				malformedTermination,
				ReceiverPeerTerminalAuthorityRemote,
				ReceiverPeerProvenanceRemoteControlMalformed,
				ReceiverPeerTerminalOperationOnly,
				ReceiverPeerProvenanceRemoteControlMalformed,
				ReceiverPeerDiagnosticControlMalformed,
			)
			if malformed.OperationID() != malformedID {
				t.Fatal("malformed peer response changed its stable operation identity")
			}
			if stopped := waitPeerOperationID(t, handler.stopped, "malformed watcher stop"); stopped != malformedID {
				t.Fatalf("sender stopped peer ID=%x, want %x", stopped, malformedID)
			}

			select {
			case <-malformedCall.doneChannel():
			default:
				t.Fatal("malformed peer cleanup retained its RPC waiter")
			}
			malformedCall.stateMu.Lock()
			callClosed, queuedResponses := malformedCall.closed, len(malformedCall.messages)
			malformedCall.stateMu.Unlock()
			if !callClosed || queuedResponses != 0 {
				t.Fatalf("malformed RPC sink closed=%v queued=%d", callClosed, queuedResponses)
			}
			malformed.mu.Lock()
			facadeClosed, stillReceiving := malformed.closed, malformed.receiving
			malformed.mu.Unlock()
			if !facadeClosed || stillReceiving {
				t.Fatalf("malformed peer facade closed=%v receiving=%v", facadeClosed, stillReceiving)
			}
			if !malformedGeneration.IsCurrent() || malformedGeneration.IsActive() {
				t.Fatalf("malformed generation current=%v active=%v",
					malformedGeneration.IsCurrent(), malformedGeneration.IsActive())
			}
			if requestKind, ok := malformedGeneration.RequestKind(); !ok || requestKind != protocolsession.MessagePeerOffer {
				t.Fatalf("malformed tombstone request kind=%d present=%v", requestKind, ok)
			}
			receiver.rpc.mu.Lock()
			retainedMalformed := receiver.rpc.calls[malformedID] != nil
			retainedSibling := receiver.rpc.calls[siblingID] == sibling.call
			activeRPCCalls := len(receiver.rpc.calls)
			receiver.rpc.mu.Unlock()
			attempts, watchers, malformedCancels := handler.snapshot(malformedID)
			if retainedMalformed || !retainedSibling || activeRPCCalls != 1 ||
				attempts != 1 || watchers != 1 || malformedCancels != 1 {
				t.Fatalf("post-malformed lifecycle badRPC=%v siblingRPC=%v calls=%d attempts=%d watchers=%d cancels=%d",
					retainedMalformed, retainedSibling, activeRPCCalls, attempts, watchers, malformedCancels)
			}
			if runtimeActive, tombstones := receiver.operations.ActiveCount(), receiver.operations.TombstoneCount(); runtimeActive != 1 || tombstones != 1 || !siblingGeneration.IsActive() {
				t.Fatalf("post-malformed operations active=%d tombstones=%d siblingActive=%v",
					runtimeActive, tombstones, siblingGeneration.IsActive())
			}

			siblingResult := sibling.Receive(context.Background())
			answer := requireReceiverPeerControl(t, siblingResult)
			if answer.Kind() != protocolsession.MessagePeerAnswer || !bytes.Equal(answer.Body(), validAnswer) {
				t.Fatalf("sibling peer response kind=%d body=%x", answer.Kind(), answer.Body())
			}
			if receiver.Err() != nil || sender.Err() != nil {
				t.Fatalf("malformed peer response escaped its operation receiver=%v sender=%v", receiver.Err(), sender.Err())
			}
			assertReceiverPeerTermination(
				t,
				sibling,
				sibling.Terminate(context.Background()),
				ReceiverPeerTerminalAuthorityLocal,
				ReceiverPeerProvenanceLocalExplicitStop,
				ReceiverPeerTerminalOperationOnly,
				ReceiverPeerProvenanceLocalExplicitStop,
			)
			if stopped := waitPeerOperationID(t, handler.stopped, "sibling watcher stop"); stopped != siblingID {
				t.Fatalf("sender stopped sibling ID=%x, want %x", stopped, siblingID)
			}
			attempts, watchers, siblingCancels := handler.snapshot(siblingID)
			_, _, malformedCancels = handler.snapshot(malformedID)
			receiver.rpc.mu.Lock()
			activeRPCCalls = len(receiver.rpc.calls)
			receiver.rpc.mu.Unlock()
			if attempts != 0 || watchers != 0 || malformedCancels != 1 || siblingCancels != 1 || activeRPCCalls != 0 {
				t.Fatalf("final peer drain attempts=%d watchers=%d badCancels=%d siblingCancels=%d calls=%d",
					attempts, watchers, malformedCancels, siblingCancels, activeRPCCalls)
			}
		})
	}
}

func TestCatalogProgressObserverFailureStaysOperationLocal(t *testing.T) {
	if err := (CatalogScanProgressObserverFunc(nil)).ObserveCatalogScanProgress(
		context.Background(), CatalogScanProgress{},
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil catalog progress observer error = %v", err)
	}
	fixture := newVerticalFixture(t)
	wantObserverErr := errors.New("progress consumer stopped")
	receiverConfig := fixture.receiverConfig
	receiverConfig.CatalogProgress = CatalogScanProgressObserverFunc(
		func(context.Context, CatalogScanProgress) error { return wantObserverErr },
	)
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	result := make(chan error, 1)
	go func() {
		_, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		result <- err
	}()
	<-fixture.scanStarted
	close(fixture.scanGate)
	if err := <-result; !errors.Is(err, wantObserverErr) {
		t.Fatalf("catalog progress observer error = %v", err)
	}
	if receiver.Err() != nil {
		t.Fatalf("operation-local observer failure terminated ProtocolSession: %v", receiver.Err())
	}
}

func TestCatalogTransportRejectsHostileOperationResponses(t *testing.T) {
	request, err := catalogflow.NewListRequest(id16[catalog.DirectoryID](91), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (rpcCatalogTransport{}).FetchPage(context.Background(), catalogflow.ListRequest{}); !errors.Is(err, catalogflow.ErrInvalidRequest) {
		t.Fatalf("invalid list request error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (rpcCatalogTransport{rpc: &rpcClient{}}).FetchPage(cancelled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list request error = %v", err)
	}

	t.Run("await cancellation remains operation local", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		harness := newBlockedCatalogFetch(t, ctx)
		cancel()
		if err := harness.wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("catalog await cancellation error = %v", err)
		}
		if harness.receiver.Err() != nil {
			t.Fatalf("catalog cancellation terminated ProtocolSession: %v", harness.receiver.Err())
		}
	})

	t.Run("malformed signed wrapper is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageScanProgress, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		assertSessionFailure(t, harness.wait())
	})

	t.Run("malformed progress semantic is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		enqueueCallResponse(harness.call, signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageScanProgress, harness.call.id, []byte{1},
		))
		err := harness.wait()
		assertSessionFailure(t, err)
		if !errors.Is(err, ErrScanProgress) {
			t.Fatalf("malformed progress error = %v", err)
		}
	})

	t.Run("duplicate progress is coalesced before unexpected final", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		semantic, err := protocolsession.EncodeScanProgress(protocolsession.ScanProgress{
			AttemptID: id16[catalog.ScanAttemptID](92), DiscoveredEntries: 256,
		})
		if err != nil {
			t.Fatal(err)
		}
		progress := signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageScanProgress, harness.call.id, semantic,
		)
		enqueueCallResponse(harness.call, progress)
		enqueueCallResponse(harness.call, progress)
		unexpected, err := protocolsession.NewMessage(
			protocolsession.MessageOpenResults, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, unexpected)
		if err := harness.wait(); !errors.Is(err, ErrOperationMissing) {
			t.Fatalf("unexpected catalog final error = %v", err)
		}
	})

	t.Run("malformed operation error is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageOperationError, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		assertSessionFailure(t, harness.wait())
	})

	t.Run("directory operation error retains its typed domain", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
			Scope: protocolsession.OperationScopeDirectory,
			Code:  catalogflow.DirectoryCodePermanentIO, Message: "directory unavailable",
		})
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageOperationError, harness.call.id, semantic,
		))
		err = harness.wait()
		var remote RemoteOperationError
		if !errors.As(err, &remote) || remote.Failure().Scope() != protocolsession.OperationScopeDirectory {
			t.Fatalf("directory operation error = %v", err)
		}
	})

	t.Run("malformed catalog result wrapper is rejected", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageCatalogResult, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		if err := harness.wait(); err == nil {
			t.Fatal("malformed catalog result crossed the transport boundary")
		}
	})
}

type blockedCatalogFetch struct {
	t        *testing.T
	fixture  *verticalFixture
	sender   *SenderRuntime
	receiver *ReceiverRuntime
	call     *operationCall
	result   <-chan error
	release  sync.Once
}

func newBlockedCatalogFetch(t *testing.T, ctx context.Context) *blockedCatalogFetch {
	t.Helper()
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	result := make(chan error, 1)
	go func() {
		request, err := catalogflow.NewListRequest(fixture.directoryID, nil, 0)
		if err == nil {
			_, err = (rpcCatalogTransport{rpc: receiver.rpc}).FetchPage(ctx, request)
		}
		result <- err
	}()
	select {
	case <-fixture.scanStarted:
	case err := <-result:
		receiver.Close()
		sender.Close()
		t.Fatalf("catalog fetch ended before the blocked scan: %v", err)
	case <-time.After(time.Second):
		receiver.Close()
		sender.Close()
		t.Fatal("catalog scan did not start")
	}
	receiver.rpc.mu.Lock()
	activeCalls := len(receiver.rpc.calls)
	if activeCalls != 1 {
		receiver.rpc.mu.Unlock()
		close(fixture.scanGate)
		receiver.Close()
		sender.Close()
		t.Fatalf("active catalog RPC calls = %d, want 1", activeCalls)
	}
	var call *operationCall
	for _, active := range receiver.rpc.calls {
		call = active
	}
	receiver.rpc.mu.Unlock()
	harness := &blockedCatalogFetch{
		t: t, fixture: fixture, sender: sender, receiver: receiver, call: call, result: result,
	}
	t.Cleanup(func() {
		harness.unblockSender()
		receiver.Close()
		sender.Close()
	})
	return harness
}

func (harness *blockedCatalogFetch) wait() error {
	harness.t.Helper()
	select {
	case err := <-harness.result:
		harness.unblockSender()
		return err
	case <-time.After(time.Second):
		harness.unblockSender()
		harness.t.Fatal("catalog fetch did not consume the injected response")
		return nil
	}
}

func (harness *blockedCatalogFetch) unblockSender() {
	harness.release.Do(func() { close(harness.fixture.scanGate) })
}

func assertSessionFailure(t *testing.T, err error) {
	t.Helper()
	var failure *transfer.SessionFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not session-fatal: %v", err)
	}
}

func enqueueCallResponse(call *operationCall, message protocolsession.Message) {
	generation, _ := call.operationAuthority()
	call.messages <- operationResponse{message: message, generation: generation}
}

func openTestPeerOperation(t *testing.T, receiver *ReceiverRuntime) *ReceiverPeerOperation {
	t.Helper()
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "offer"})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := receiver.OpenPeerOperation(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func signedPeerOperationControl(
	t *testing.T,
	receiver *ReceiverRuntime,
	privateKey ed25519.PrivateKey,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	semantic []byte,
) protocolsession.Message {
	t.Helper()
	laneID, laneEpoch := receiver.LaneIdentity()
	binding := receiver.senderControlBase(LaneIdentity{ID: laneID, Epoch: laneEpoch})
	binding.Sequence = 1
	binding.MessageKind = kind
	binding.OperationID = operationID
	binding.HasOperationID = true
	signed, err := protocolsession.SignControlBody(
		privateKey, protocolsession.ControlDomainOperation, binding, semantic,
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(kind, &operationID, signed)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

type trackedSenderPeerAttempt struct {
	done chan struct{}
}

type trackedSenderPeerHandler struct {
	session     SenderPeerSession
	validAnswer []byte
	offers      chan protocolsession.OperationID
	stopped     chan protocolsession.OperationID

	mu          sync.Mutex
	stopping    bool
	offerCount  int
	attempts    map[protocolsession.OperationID]*trackedSenderPeerAttempt
	cancelCalls map[protocolsession.OperationID]int
	watchers    sync.WaitGroup
	active      atomic.Int32
}

func newTrackedSenderPeerHandler(
	session SenderPeerSession,
	validAnswer []byte,
) *trackedSenderPeerHandler {
	return &trackedSenderPeerHandler{
		session: session, validAnswer: bytes.Clone(validAnswer),
		offers: make(chan protocolsession.OperationID, 4), stopped: make(chan protocolsession.OperationID, 4),
		attempts:    make(map[protocolsession.OperationID]*trackedSenderPeerAttempt),
		cancelCalls: make(map[protocolsession.OperationID]int),
	}
}

func (handler *trackedSenderPeerHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operationID, ok := message.OperationID()
	if !ok {
		return ErrRuntimeConfig
	}
	if message.Kind() == protocolsession.MessagePeerCandidate {
		return nil
	}
	if message.Kind() != protocolsession.MessagePeerOffer {
		return ErrRuntimeConfig
	}
	attempt := &trackedSenderPeerAttempt{done: make(chan struct{})}
	handler.mu.Lock()
	if handler.stopping || handler.attempts[operationID] != nil {
		handler.mu.Unlock()
		return ErrRuntimeClosed
	}
	handler.offerCount++
	offerIndex := handler.offerCount
	handler.attempts[operationID] = attempt
	handler.watchers.Add(1)
	handler.active.Add(1)
	handler.mu.Unlock()
	go func() {
		defer handler.watchers.Done()
		<-attempt.done
		handler.active.Add(-1)
		handler.stopped <- operationID
	}()
	handler.offers <- operationID
	if offerIndex == 1 {
		return nil
	}
	_, err := handler.session.SendPeerControl(
		ctx, protocolsession.MessagePeerAnswer, operationID, handler.validAnswer,
	)
	return err
}

func (handler *trackedSenderPeerHandler) Cancel(
	_ context.Context,
	operationID protocolsession.OperationID,
) error {
	handler.mu.Lock()
	handler.cancelCalls[operationID]++
	attempt := handler.attempts[operationID]
	delete(handler.attempts, operationID)
	handler.mu.Unlock()
	if attempt != nil {
		close(attempt.done)
	}
	return nil
}

func (handler *trackedSenderPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	handler.mu.Lock()
	handler.stopping = true
	attempts := make([]*trackedSenderPeerAttempt, 0, len(handler.attempts))
	for operationID, attempt := range handler.attempts {
		attempts = append(attempts, attempt)
		delete(handler.attempts, operationID)
	}
	handler.mu.Unlock()
	for _, attempt := range attempts {
		close(attempt.done)
	}
	handler.watchers.Wait()
	return ctx.Err()
}

func (handler *trackedSenderPeerHandler) snapshot(
	operationID protocolsession.OperationID,
) (attempts int, watchers int32, cancels int) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return len(handler.attempts), handler.active.Load(), handler.cancelCalls[operationID]
}

func waitPeerOperationID(
	t *testing.T,
	events <-chan protocolsession.OperationID,
	label string,
) protocolsession.OperationID {
	t.Helper()
	select {
	case operationID := <-events:
		return operationID
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return protocolsession.OperationID{}
	}
}
