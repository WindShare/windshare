package sendersession

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/catalog"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
	"testing"
	"time"
)

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
				protocolsession.OperationLimits{MaxActive: 16, MaxTracked: 16}, nil,
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
	handler := newCatalogHandler(nil, sessioncore.Responses{})
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
	); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("empty peer control error = %v", err)
	}
	if _, err := empty.AdmitPeerChannel(
		context.Background(), nil, allowSenderPeerSettlement,
	); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("empty peer admission error = %v", err)
	}
	if err := empty.FailPeerOperation(
		context.Background(), protocolsession.OperationID{},
		protocolsession.PeerOperationCodeNegotiation, "missing",
	); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("empty peer failure error = %v", err)
	}
	if _, err := (PeerHandlerFactoryFunc(nil)).NewSenderPeerHandler(empty); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
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
