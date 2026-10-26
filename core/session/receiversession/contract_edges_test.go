package receiversession

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/content"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type receiverResourceSourceFunc func() (RuntimeResourceLease, error)

func (source receiverResourceSourceFunc) AcquireReceiverRuntimeResources() (
	RuntimeResourceLease,
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
	for name, source := range map[string]RuntimeResourceSource{
		"acquire failure": receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
			return nil, wantAcquireErr
		}),
		"nil lease": receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
			return nil, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			factory := &Factory{resources: source}
			_, err := factory.acquireRuntimeResources()
			if name == "acquire failure" && !errors.Is(err, wantAcquireErr) {
				t.Fatalf("resource acquisition error = %v", err)
			}
			if name == "nil lease" && !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
				t.Fatalf("nil resource lease error = %v", err)
			}
		})
	}
	connectSourceCalls := atomic.Int32{}
	connectConfig := fixture.receiverConfig
	connectConfig.RuntimeResources = receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
		connectSourceCalls.Add(1)
		return nil, wantAcquireErr
	})
	connectFactory, err := NewFactory(connectConfig)
	if err == nil {
		trackReceiverFactory(connectFactory)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connectFactory.Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, wantAcquireErr) {
		t.Fatalf("Connect resource acquisition error = %v", err)
	}
	if connectSourceCalls.Load() != 1 {
		t.Fatalf("Connect acquired resources %d times", connectSourceCalls.Load())
	}

	failedLease := &countedReceiverResourceLease{}
	failureConfig := fixture.receiverConfig
	failureConfig.Random = edgeErrorReader{}
	failureConfig.RuntimeResources = receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
		return failedLease, nil
	})
	failureFactory, err := NewFactory(failureConfig)
	if err == nil {
		trackReceiverFactory(failureFactory)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failureFactory.Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("handshake failure with resource lease = %v", err)
	}
	if failedLease.releases.Load() != 1 {
		t.Fatalf("failed connection released resources %d times", failedLease.releases.Load())
	}

	activeLease := &countedReceiverResourceLease{}
	successConfig := fixture.receiverConfig
	successConfig.RuntimeResources = receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
		return activeLease, nil
	})
	successFactory, err := NewFactory(successConfig)
	if err == nil {
		trackReceiverFactory(successFactory)
	}
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
	receiverConfig.RuntimeResources = receiverResourceSourceFunc(func() (RuntimeResourceLease, error) {
		return receiverLease, nil
	})
	receiverFactory, err := NewFactory(receiverConfig)
	if err == nil {
		trackReceiverFactory(receiverFactory)
	}
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan verticalLeaseEnd, 1)
	fixture.contentStore.ended = ended
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
	case ending := <-ended:
		if ending.id != fixture.contentStore.lease.ID() || ending.kind != content.LeaseDetached {
			t.Fatalf("sender lease ending=%+v", ending)
		}
	default:
		t.Fatal("sender Done retained its per-session content lease")
	}
	trackedRequests(receiver.rpc).mu.Lock()
	rpcClosed, calls := trackedRequests(receiver.rpc).closed, len(trackedRequests(receiver.rpc).calls)
	trackedRequests(receiver.rpc).mu.Unlock()
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
		factory, err := NewFactory(config)
		if err == nil {
			trackReceiverFactory(factory)
		}
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := factory.Connect(ctx, newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, io.ErrUnexpectedEOF) {
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
			_, err := fixture.receiverFactory.Connect(ctx, receiverChannel, transferlanes.RouteRelay)
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

func TestReceiverPeerOperationRejectsMalformedAndCrossScopeFinals(t *testing.T) {
	if _, err := (*Runtime)(nil).OpenPeerOperation(context.Background(), nil); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil receiver peer operation error = %v", err)
	}
	var nilOperation *PeerOperation
	if !nilOperation.OperationID().IsZero() {
		t.Fatal("nil peer operation exposed an identity")
	}
	if _, err := nilOperation.SendCandidate(context.Background(), nil); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
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
	receiverFactory, err := NewFactory(receiverConfig)
	if err == nil {
		trackReceiverFactory(receiverFactory)
	}
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	t.Run("unexpected response kind", func(t *testing.T) {
		operation := openTestPeerOperation(t, receiver)
		message, err := protocolsession.NewMessage(
			protocolsession.MessageCatalogResult, operationIDPointer(operation.call.ID()), []byte{1},
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
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteUnknownControl,
			PeerTerminalOperationOnly,
			PeerProvenanceRemoteUnknownControl,
			PeerDiagnosticUnknownControl,
		)
		if receiverPeerDiagnosticsContain(termination.Diagnostics(), PeerDiagnosticOperationMissing) {
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
			t, receiver, fixture.senderConfig.SenderPrivateKey,
			protocolsession.MessageOperationError, operation.call.ID(), semantic,
		))
		termination := requireReceiverPeerTermination(t, operation.Receive(context.Background()))
		assertReceiverPeerTermination(
			t,
			operation,
			termination,
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteFailureScopeViolation,
			PeerTerminalSessionUnsafe,
			PeerProvenanceRemoteFailureScopeViolation,
			PeerDiagnosticRemoteFailureScopeViolation,
		)
		failure := requireReceiverPeerRemoteFailure(
			t,
			termination,
			PeerDiagnosticRemoteFailureScopeViolation,
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
			PeerTerminalAuthorityLocal,
			PeerProvenanceLocalContextEnded,
			PeerTerminalOperationOnly,
			PeerProvenanceLocalContextEnded,
			PeerDiagnosticContextCanceled,
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
			fixture.senderConfig.Peers = sendersession.PeerHandlerFactoryFunc(func(
				session sendersession.PeerSession,
			) (sendersession.PeerHandler, error) {
				handler := newTrackedSenderPeerHandler(session, validAnswer)
				created <- handler
				return handler, nil
			})
			fixture.rebuildSender(t)
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
			receiverFactory, err := NewFactory(receiverConfig)
			if err == nil {
				trackReceiverFactory(receiverFactory)
			}
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
			malformedGeneration, _ := malformedCall.OperationAuthority()
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
			siblingGeneration, _ := sibling.call.OperationAuthority()

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
				PeerTerminalAuthorityRemote,
				PeerProvenanceRemoteControlMalformed,
				PeerTerminalOperationOnly,
				PeerProvenanceRemoteControlMalformed,
				PeerDiagnosticControlMalformed,
			)
			if malformed.OperationID() != malformedID {
				t.Fatal("malformed peer response changed its stable operation identity")
			}
			if stopped := waitPeerOperationID(t, handler.stopped, "malformed watcher stop"); stopped != malformedID {
				t.Fatalf("sender stopped peer ID=%x, want %x", stopped, malformedID)
			}

			retiredGeneration, retiredAuthority := malformedCall.OperationAuthority()
			callClosed, queuedResponses := retiredGeneration.IsZero() && retiredAuthority.IsZero(), 0
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
			trackedRequests(receiver.rpc).mu.Lock()
			retainedMalformed := trackedRequests(receiver.rpc).calls[malformedID] != nil
			retainedSibling := trackedRequests(receiver.rpc).calls[siblingID] == sibling.call
			activeRPCCalls := len(trackedRequests(receiver.rpc).calls)
			trackedRequests(receiver.rpc).mu.Unlock()
			attempts, watchers, malformedCancels := handler.snapshot(malformedID)
			if retainedMalformed || !retainedSibling || activeRPCCalls != 1 ||
				attempts != 1 || watchers != 1 || malformedCancels != 1 {
				t.Fatalf("post-malformed lifecycle badRPC=%v siblingRPC=%v calls=%d attempts=%d watchers=%d cancels=%d",
					retainedMalformed, retainedSibling, activeRPCCalls, attempts, watchers, malformedCancels)
			}
			if !siblingGeneration.IsActive() {
				t.Fatal("malformed operation revoked its sibling")
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
				PeerTerminalAuthorityLocal,
				PeerProvenanceLocalExplicitStop,
				PeerTerminalOperationOnly,
				PeerProvenanceLocalExplicitStop,
			)
			if stopped := waitPeerOperationID(t, handler.stopped, "sibling watcher stop"); stopped != siblingID {
				t.Fatalf("sender stopped sibling ID=%x, want %x", stopped, siblingID)
			}
			attempts, watchers, siblingCancels := handler.snapshot(siblingID)
			_, _, malformedCancels = handler.snapshot(malformedID)
			trackedRequests(receiver.rpc).mu.Lock()
			activeRPCCalls = len(trackedRequests(receiver.rpc).calls)
			trackedRequests(receiver.rpc).mu.Unlock()
			if attempts != 0 || watchers != 0 || malformedCancels != 1 || siblingCancels != 1 || activeRPCCalls != 0 {
				t.Fatalf("final peer drain attempts=%d watchers=%d badCancels=%d siblingCancels=%d calls=%d",
					attempts, watchers, malformedCancels, siblingCancels, activeRPCCalls)
			}
		})
	}
}

func assertSessionFailure(t *testing.T, err error) {
	t.Helper()
	if !isSessionTerminalBoundaryForTest(err) {
		t.Fatalf("error is not session-fatal: %v", err)
	}
}

func openTestPeerOperation(t *testing.T, receiver *Runtime) *PeerOperation {
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
	receiver *Runtime,
	privateKey ed25519.PrivateKey,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	semantic []byte,
) protocolsession.Message {
	t.Helper()
	laneID, laneEpoch := receiver.core.LaneIdentity()
	binding := protocolsession.ControlBinding{ShareInstance: receiver.core.ShareInstance(), ProtocolSessionID: receiver.core.ProtocolSessionID(), LaneID: laneID, LaneEpoch: laneEpoch, Direction: protocolsession.DirectionSenderToReceiver}
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
	session     sendersession.PeerSession
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
	session sendersession.PeerSession,
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
		return sessioncontract.ErrRuntimeConfig
	}
	if message.Kind() == protocolsession.MessagePeerCandidate {
		return nil
	}
	if message.Kind() != protocolsession.MessagePeerOffer {
		return sessioncontract.ErrRuntimeConfig
	}
	attempt := &trackedSenderPeerAttempt{done: make(chan struct{})}
	handler.mu.Lock()
	if handler.stopping || handler.attempts[operationID] != nil {
		handler.mu.Unlock()
		return sessioncontract.ErrRuntimeClosed
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
