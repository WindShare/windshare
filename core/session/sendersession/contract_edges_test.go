package sendersession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type stubHandlerRegistrar struct {
	handlers map[protocolsession.MessageKind]protocolsession.MessageHandler
}

func (r *stubHandlerRegistrar) RegisterHandler(kind protocolsession.MessageKind, h protocolsession.MessageHandler) error {
	if r.handlers == nil {
		r.handlers = make(map[protocolsession.MessageKind]protocolsession.MessageHandler)
	}
	if _, exists := r.handlers[kind]; exists {
		return protocolsession.ErrHandlerRegistered
	}
	r.handlers[kind] = h
	return nil
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
			router := &stubHandlerRegistrar{}
			if err := router.RegisterHandler(duplicate, protocolsession.MessageHandlerFunc(
				func(context.Context, protocolsession.Operation, protocolsession.Message) error { return nil },
			)); err != nil {
				t.Fatal(err)
			}
			err := registerSenderHandlers(
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
	handler := newCatalogHandler(nil, &sessioncore.Responses{})
	for range cap(handler.workers) {
		handler.workers <- struct{}{}
	}
	duplicateID := protocolsession.OperationID(61)
	queuedID := protocolsession.OperationID(62)
	barrierID := protocolsession.OperationID(63)
	listMessage := func(operationID protocolsession.OperationID) protocolsession.Message {
		message, err := protocolsession.NewMessage(
			protocolsession.MessageListChildren, &operationID, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	state := newStubOperationState()
	duplicateMessage := listMessage(duplicateID)
	duplicateOp := state.admit(duplicateID, protocolsession.MessageListChildren)
	if !handler.add(duplicateOp.ID(), func() {}) {
		t.Fatal("failed to seed active catalog operation")
	}
	queuedMessage := listMessage(queuedID)
	queuedOp := state.admit(queuedID, protocolsession.MessageListChildren)
	barrierOp := state.admit(barrierID, protocolsession.MessageListChildren)
	barrierReached := make(chan struct{})
	var barrierOnce sync.Once
	if !handler.add(barrierOp.ID(), func() { barrierOnce.Do(func() { close(barrierReached) }) }) {
		t.Fatal("failed to seed catalog barrier")
	}
	for _, queued := range []struct {
		op      protocolsession.Operation
		message protocolsession.Message
	}{
		{duplicateOp, duplicateMessage},
		{queuedOp, queuedMessage},
		{queuedOp, operationMessageForTest(t, protocolsession.MessageCancel, queuedID, validCancelBody(t))},
		{barrierOp, operationMessageForTest(t, protocolsession.MessageCancel, barrierID, validCancelBody(t))},
	} {
		if err := handler.HandleMessage(context.Background(), queued.op, queued.message); err != nil {
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
	if err := handler.progressObserver(queuedOp).ObserveScanProgress(
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
		context.Background(), protocolsession.Operation{}, protocolsession.MessagePeerAnswer, nil,
	); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("empty peer control error = %v", err)
	}
	if _, err := empty.AdmitPeerChannel(
		context.Background(), nil, allowSenderPeerSettlement,
	); !errors.Is(err, session.ErrRuntimeClosed) {
		t.Fatalf("empty peer admission error = %v", err)
	}
	if err := empty.FailPeerOperation(
		context.Background(), protocolsession.Operation{},
		protocolsession.PeerOperationCodeNegotiation, "missing",
	); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("empty peer failure error = %v", err)
	}
	if _, err := (PeerHandlerFactoryFunc(nil)).NewSenderPeerHandler(empty); !errors.Is(err, session.ErrRuntimeConfig) {
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
