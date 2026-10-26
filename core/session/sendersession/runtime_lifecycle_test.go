package sendersession

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	sessioncontract "github.com/windshare/windshare/core/session"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"testing"
	"time"
)

func TestRuntimeCleanCloseDrainsFullServiceQueue(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	handler := newCatalogHandler(nil, &sessioncore.Responses{})
	for range cap(handler.queue) {
		// Zero generations model work already invalidated by the same terminal
		// transition; Run must suppress it without touching the closed service.
		handler.queue <- catalogOperation{}
	}
	runtime.Start(sessioncore.Component{Source: sessioncore.RuntimeFailureSourceCatalog, Run: handler.Run})
	runtime.Close()
	if runtime.Err() != nil {
		t.Fatalf("clean close retained component error=%v", runtime.Err())
	}
	if len(handler.queue) != 0 {
		t.Fatalf("clean close retained %d service queue entries", len(handler.queue))
	}
	handler.queueMu.Lock()
	stopping := handler.stopping
	handler.queueMu.Unlock()
	handler.mu.Lock()
	active := len(handler.active)
	handler.mu.Unlock()
	if !stopping || active != 0 {
		t.Fatalf("handler stopping=%v active=%d", stopping, active)
	}
	select {
	case <-runtime.Done():
	default:
		t.Fatal("role service outlived runtime closure")
	}

}

func TestServiceHandlersEnforceBoundedQueuesAndSharedDirectoryFailureSchema(t *testing.T) {
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	operationID := id16[protocolsession.OperationID](81)
	list, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &operationID, body)
	laneBody, _ := sessioncore.EncodeLaneAttachRequest(0)
	laneRequest, _ := protocolsession.NewMessage(protocolsession.MessageLaneAttach, &operationID, laneBody)

	catalogHandler := newCatalogHandler(nil, &sessioncore.Responses{})
	if err := catalogHandler.HandleMessage(context.Background(), terminal); !errors.Is(err, contentflow.ErrUnexpectedMessage) {
		t.Fatalf("catalog wrong-kind error = %v", err)
	}
	listContext := senderIngressContext(t, list)
	for range cap(catalogHandler.queue) {
		if err := catalogHandler.HandleMessage(listContext, list); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalogHandler.HandleMessage(listContext, list); !errors.Is(err, contentflow.ErrServiceQueueFull) {
		t.Fatalf("catalog queue overflow error = %v", err)
	}

	laneHandler := newLaneGrantHandler(nil, &sessioncore.Responses{}, edgeErrorReader{})
	if err := laneHandler.HandleMessage(context.Background(), terminal); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("lane wrong-kind error = %v", err)
	}
	laneContext := senderIngressContext(t, laneRequest)
	for range cap(laneHandler.queue) {
		if err := laneHandler.HandleMessage(laneContext, laneRequest); err != nil {
			t.Fatal(err)
		}
	}
	if err := laneHandler.HandleMessage(laneContext, laneRequest); !errors.Is(err, sessioncontract.ErrOperationOverflow) {
		t.Fatalf("lane queue overflow error = %v", err)
	}
	malformedHandler := newLaneGrantHandler(nil, &sessioncore.Responses{}, edgeErrorReader{})
	malformed, _ := protocolsession.NewMessage(protocolsession.MessageLaneAttach, &operationID, body)
	if err := malformedHandler.HandleMessage(senderIngressContext(t, malformed), malformed); err != nil {
		t.Fatal(err)
	}
	if err := malformedHandler.Run(context.Background()); err == nil {
		t.Fatal("malformed queued lane request did not fail its handler")
	}
	if err := laneHandler.process(context.Background(), laneRequest); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lane grant nonce error = %v", err)
	}

	var cancelled bool
	cancelID := id16[protocolsession.OperationID](82)
	cancelKey := catalogOperationKey{id: cancelID}
	catalogHandler.active[cancelKey] = func() { cancelled = true }
	catalogHandler.cancel(cancelKey)
	if !cancelled || catalogHandler.add(cancelKey, func() {}) {
		t.Fatal("catalog operation cancellation lost duplicate authority")
	}
	catalogHandler.remove(cancelKey)
	if err := (cancelMux{catalog: catalogHandler}).HandleMessage(context.Background(), terminal); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("cancel without operation error = %v", err)
	}

}

func newUnstartedRuntime(t *testing.T, role protocolsession.Role) (*sessioncore.Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithPolicy(t, role, protocolsession.OperationLimits{}, nil)
}

func newUnstartedRuntimeWithPolicy(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
) (*sessioncore.Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithContinuations(t, role, limits, now, nil)
}

func newUnstartedRuntimeWithContinuations(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
	continuations protocolsession.OperationContinuationClassifier,
) (*sessioncore.Runtime, *memoryChannel) {
	t.Helper()
	share := id16[catalog.ShareInstance](91)
	authKey := bytes.Repeat([]byte{92}, protocolsession.SessionAuthKeyBytes)
	receiverPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: 93})
	if err != nil {
		t.Fatal(err)
	}
	client, err := protocolsession.NewClientHello(
		share, id16[protocolsession.ReceiverInstanceID](94),
		bytes.Repeat([]byte{95}, protocolsession.HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := protocolsession.NewClientHelloReplayGuard(8, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err = replay.AcceptClientHello(client.Encoded(), share, authKey)
	if err != nil {
		t.Fatal(err)
	}
	senderPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: 96})
	if err != nil {
		t.Fatal(err)
	}
	signingKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize))
	server, err := protocolsession.NewServerHello(
		client, bytes.Repeat([]byte{98}, protocolsession.HandshakeNonceBytes),
		senderPrivate.PublicKey(), 1, signingKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	var keys protocolsession.SessionKeys
	if role == protocolsession.RoleSender {
		keys, err = protocolsession.DeriveSenderSession(senderPrivate, authKey, client, server)
	} else {
		keys, err = protocolsession.DeriveReceiverSession(receiverPrivate, authKey, client, server)
	}
	if err != nil {
		t.Fatal(err)
	}
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	runtime, err := sessioncore.New(sessioncore.Config{
		Share: share, Role: role, Keys: keys, LaneID: 1, Channel: channel,
		Random: &deterministicReader{next: 99}, Authenticator: permissiveInboundAuthenticator(),
		Continuations: continuations, OperationLimits: limits, Now: now,
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	t.Cleanup(runtime.AbortBeforeStart)
	return runtime, channel
}

func permissiveInboundAuthenticator() protocolsession.InboundMessageAuthenticator {
	return protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
}
