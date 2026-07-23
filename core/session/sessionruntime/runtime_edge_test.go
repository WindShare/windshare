package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRuntimeDependencyAdaptersAndStableErrors(t *testing.T) {
	var stopped, cleaned atomic.Int32
	connectivity := TerminalConnectivityFuncs{
		StopRecoveryFunc: func() { stopped.Add(1) },
		CleanupFunc: func(context.Context) error {
			cleaned.Add(1)
			return nil
		},
	}
	connectivity.StopRecovery()
	if err := connectivity.Cleanup(context.Background()); err != nil || stopped.Load() != 1 || cleaned.Load() != 1 {
		t.Fatalf("connectivity adapters = %d/%d, %v", stopped.Load(), cleaned.Load(), err)
	}
	TerminalConnectivityFuncs{}.StopRecovery()
	if err := (TerminalConnectivityFuncs{}).Cleanup(context.Background()); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil cleanup error = %v", err)
	}
	if _, err := (SenderContentFactoryFunc(nil)).NewSenderContentService(); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil content factory error = %v", err)
	}
	if _, err := (InitialLaneIDSourceFunc(nil)).NextInitialLaneID(); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil lane source error = %v", err)
	}
	if _, err := (ReceiverInstanceSourceFunc(nil)).NewReceiverInstanceID(); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil receiver source error = %v", err)
	}

	first := errors.New("first")
	leaseContext, stopLease := context.WithCancel(context.Background())
	state := &remoteLeaseState{ctx: leaseContext, cancel: stopLease}
	state.setError(first)
	state.setError(errors.New("second"))
	if !errors.Is(state.Err(), first) {
		t.Fatalf("lease retained error = %v", state.Err())
	}
	state.close()
	state.close()
	if leaseContext.Err() == nil {
		t.Fatal("lease close did not cancel its in-flight renew authority")
	}
	if (&RemoteRevisionError{}).Error() == "" || (RemoteOperationError{}).Error() == "" ||
		(&LaneRejectedError{}).Error() == "" {
		t.Fatal("typed remote errors lost stable diagnostics")
	}

	if (*ReceiverRuntime)(nil).DetachLane(LaneIdentity{}) || (*SenderRuntime)(nil).DetachLane(LaneIdentity{}) ||
		(*ReceiverRuntime)(nil).AttachedLanes() != 0 || (*SenderRuntime)(nil).AttachedLanes() != 0 {
		t.Fatal("nil lane accessors were not inert")
	}
	(*ReceiverRuntime)(nil).Close()
	(*SenderRuntime)(nil).Close()
	if err := (*SenderRuntime)(nil).Stop(context.Background(), "stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil sender stop error = %v", err)
	}
	if err := (*SenderFactory)(nil).Stop(context.Background(), "stop"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil factory stop error = %v", err)
	}
}

func TestFactoryDefaultsValidationAndHandshakeFailures(t *testing.T) {
	fixture := newVerticalFixture(t)
	baseSender := senderConfigFromFixture(fixture)
	defaulted := baseSender
	defaulted.Random = nil
	defaulted.ReplayGuard = nil
	defaulted.InitialLaneIDs = nil
	factory, err := NewSenderFactory(defaulted)
	if err != nil {
		t.Fatal(err)
	}
	if err := factory.Stop(context.Background(), "unused factory"); err != nil {
		t.Fatal(err)
	}
	if err := factory.Stop(context.Background(), "repeat"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*SenderFactoryConfig){
		"zero share":      func(config *SenderFactoryConfig) { config.ShareInstance = catalog.ShareInstance{} },
		"missing auth":    func(config *SenderFactoryConfig) { config.SessionAuthKey = nil },
		"missing key":     func(config *SenderFactoryConfig) { config.SenderPrivateKey = nil },
		"missing catalog": func(config *SenderFactoryConfig) { config.Catalog = nil },
		"missing content": func(config *SenderFactoryConfig) { config.Content = nil },
		"missing cleanup": func(config *SenderFactoryConfig) { config.TerminalConnectivity = nil },
		"negative timeout": func(config *SenderFactoryConfig) {
			config.TerminalTimeout = -time.Second
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := baseSender
			mutate(&config)
			if _, err := NewSenderFactory(config); !errors.Is(err, ErrRuntimeConfig) {
				t.Fatalf("factory error = %v", err)
			}
		})
	}

	receiverDefaults := fixture.receiverConfig
	receiverDefaults.Random = nil
	receiverDefaults.ReceiverInstances = nil
	receiverDefaults.After = nil
	if _, err := NewReceiverFactory(receiverDefaults); err != nil {
		t.Fatalf("receiver defaults: %v", err)
	}
	badReceiver := fixture.receiverConfig
	badReceiver.SessionAuthKey = nil
	if _, err := NewReceiverFactory(badReceiver); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("invalid receiver error = %v", err)
	}
	if _, err := (*ReceiverFactory)(nil).Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil receiver connect error = %v", err)
	}
	if _, err := fixture.receiverFactory.Connect(context.Background(), nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil receiver channel error = %v", err)
	}
	if _, err := (*SenderFactory)(nil).Accept(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil sender accept error = %v", err)
	}
	if _, err := fixture.senderFactory.Accept(context.Background(), nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil sender channel error = %v", err)
	}

	randomFailure := fixture.receiverConfig
	randomFailure.Random = edgeErrorReader{}
	randomFactory, _ := NewReceiverFactory(randomFailure)
	if _, err := randomFactory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrHandshake) {
		t.Fatalf("receiver random error = %v", err)
	}
	instanceFailure := fixture.receiverConfig
	instanceFailure.ReceiverInstances = ReceiverInstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
		return protocolsession.ReceiverInstanceID{}, io.ErrUnexpectedEOF
	})
	instanceFactory, _ := NewReceiverFactory(instanceFailure)
	if _, err := instanceFactory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrHandshake) {
		t.Fatalf("receiver identity error = %v", err)
	}
	zeroInstance := fixture.receiverConfig
	zeroInstance.ReceiverInstances = ReceiverInstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
		return protocolsession.ReceiverInstanceID{}, nil
	})
	zeroFactory, _ := NewReceiverFactory(zeroInstance)
	if _, err := zeroFactory.Connect(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrHandshake) {
		t.Fatalf("zero receiver identity error = %v", err)
	}

	closed, closedPeer := newMemoryChannelPair()
	_ = closed.Close()
	t.Cleanup(func() { _ = closedPeer.Close() })
	if _, err := fixture.receiverFactory.Connect(context.Background(), closed); !errors.Is(err, ErrHandshake) {
		t.Fatalf("receiver send error = %v", err)
	}
	invalidServer, invalidClient := newMemoryChannelPair()
	go func() {
		<-invalidServer.Recv()
		_ = invalidServer.Send(context.Background(), framechannel.Frame{1})
	}()
	if _, err := fixture.receiverFactory.Connect(context.Background(), invalidClient); !errors.Is(err, ErrHandshake) {
		t.Fatalf("invalid server hello error = %v", err)
	}
	_ = invalidServer.Close()
	_ = invalidClient.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.senderFactory.Accept(cancelled, newMemoryChannel(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("sender receive cancellation = %v", err)
	}
	malformedSender, malformedPeer := newMemoryChannelPair()
	_ = malformedPeer.Send(context.Background(), framechannel.Frame{1})
	if _, err := fixture.senderFactory.Accept(context.Background(), malformedSender); !errors.Is(err, ErrHandshake) {
		t.Fatalf("malformed client hello error = %v", err)
	}
	_ = malformedSender.Close()
	_ = malformedPeer.Close()

	testSenderAcceptDependencyFailures(t, fixture, baseSender)
}

func TestRPCClientBoundsLifecycleAndCancellation(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	terminal, _ := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err := receiver.rpc.HandleMessage(context.Background(), terminal); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("message without operation error = %v", err)
	}
	missingID := id16[protocolsession.OperationID](71)
	missing, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &missingID, body)
	if err := receiver.rpc.HandleMessage(context.Background(), missing); err != nil {
		t.Fatalf("late response error = %v", err)
	}
	bounded := &operationCall{id: missingID, messages: make(chan operationResponse, 1)}
	seedRequest, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &missingID, body)
	requestAdmission, err := receiver.router.AdmitOutbound(seedRequest, protocolsession.OutboundOperationPermit{})
	if err != nil || !bounded.setAuthority(requestAdmission.Generation, requestAdmission.Operation) {
		t.Fatalf("seed bounded call authority: %v", err)
	}
	responseContext := protocolsession.WithOperationGeneration(context.Background(), requestAdmission.Generation)
	receiver.rpc.mu.Lock()
	receiver.rpc.calls[missingID] = bounded
	receiver.rpc.mu.Unlock()
	if err := receiver.rpc.HandleMessage(responseContext, missing); err != nil {
		t.Fatal(err)
	}
	if err := receiver.rpc.HandleMessage(responseContext, missing); !errors.Is(err, ErrOperationOverflow) {
		t.Fatalf("response overflow error = %v", err)
	}
	wrong := &operationCall{id: missingID, messages: make(chan operationResponse, 1)}
	receiver.rpc.end(wrong)
	receiver.rpc.end(nil)
	receiver.rpc.end(bounded)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.rpc.begin(cancelled, protocolsession.MessageListChildren, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin cancelled error = %v", err)
	}
	errorIDs := newRPCClient(receiver.runtimeCore, edgeErrorReader{})
	if _, err := errorIDs.begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("operation ID source error = %v", err)
	}
	zeroIDs := newRPCClient(receiver.runtimeCore, bytes.NewReader(make([]byte, protocolsession.IdentityBytes*4)))
	if _, err := zeroIDs.begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("zero operation IDs error = %v", err)
	}
	invalidMessage := newRPCClient(receiver.runtimeCore, bytes.NewReader(bytes.Repeat([]byte{7}, protocolsession.IdentityBytes)))
	if _, err := invalidMessage.begin(context.Background(), protocolsession.MessageKind(255), body); err == nil {
		t.Fatal("invalid operation kind was accepted")
	}
	duplicate := newRPCClient(receiver.runtimeCore, bytes.NewReader(bytes.Repeat([]byte{8}, protocolsession.IdentityBytes)))
	duplicateID := issuedOperationID(8, 1)
	duplicate.calls[duplicateID] = &operationCall{id: duplicateID}
	if _, err := duplicate.begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("duplicate local operation error = %v", err)
	}
	if err := receiver.rpc.register(receiver.router); err == nil {
		t.Fatal("duplicate router registration was accepted")
	}
	if err := receiver.rpc.admitCancellation(nil, contentflow.CancelReasonUser); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("nil cancellation call error = %v", err)
	}
	if err := receiver.rpc.admitCancellation(
		&operationCall{id: missingID}, contentflow.CancelReason(0),
	); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("missing cancellation authority error = %v", err)
	}

	request, _ := catalogflow.NewListRequest(fixture.directoryID, nil, 0)
	requestBody, _ := catalogflow.EncodeListRequest(request)
	call, err := receiver.rpc.begin(context.Background(), protocolsession.MessageListChildren, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.scanStarted:
	case <-time.After(time.Second):
		t.Fatal("catalog operation did not start")
	}
	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := receiver.rpc.await(waitContext, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("await cancellation = %v", err)
	}
	receiver.rpc.end(call)

	doneCall := &operationCall{messages: make(chan operationResponse)}
	receiver.Close()
	if _, err := receiver.rpc.await(context.Background(), doneCall); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("await closed runtime = %v", err)
	}
}

func TestReceiverRevisionFailureAndDuplicateLeasePaths(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	unknownFile := id16[catalog.FileID](99)
	if _, err := receiver.OpenRevision(context.Background(), unknownFile); err == nil {
		t.Fatal("unknown revision was opened")
	} else {
		var remote *RemoteRevisionError
		if !errors.As(err, &remote) || remote.Error() == "" {
			t.Fatalf("unknown revision error = %v", err)
		}
	}
	unknownLease := id16[content.LeaseID](98)
	if err := receiver.ReleaseRevision(context.Background(), unknownLease); err != nil {
		t.Fatalf("idempotent unknown release = %v", err)
	}
	if err := receiver.revisions.leaseError(unknownLease); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("unknown lease state error = %v", err)
	}
	first, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); err == nil {
		t.Fatal("duplicate lease was accepted")
	} else {
		var remote RemoteOperationError
		if !errors.As(err, &remote) {
			t.Fatalf("duplicate lease error = %v", err)
		}
	}
	if err := receiver.ReleaseRevision(context.Background(), first.LeaseID); err != nil {
		t.Fatal(err)
	}
}

func testSenderAcceptDependencyFailures(t *testing.T, fixture *verticalFixture, base SenderFactoryConfig) {
	t.Helper()
	tests := []struct {
		name   string
		mutate func(*SenderFactoryConfig)
	}{
		{"random", func(config *SenderFactoryConfig) { config.Random = edgeErrorReader{} }},
		{"lane id", func(config *SenderFactoryConfig) {
			config.InitialLaneIDs = InitialLaneIDSourceFunc(func() (uint32, error) { return 0, io.ErrUnexpectedEOF })
		}},
		{"zero lane id", func(config *SenderFactoryConfig) {
			config.InitialLaneIDs = InitialLaneIDSourceFunc(func() (uint32, error) { return 0, nil })
		}},
		{"catalog", func(config *SenderFactoryConfig) {
			config.Catalog = SenderCatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return nil, io.ErrUnexpectedEOF
			})
		}},
		{"nil catalog", func(config *SenderFactoryConfig) {
			config.Catalog = SenderCatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return nil, nil
			})
		}},
		{"content", func(config *SenderFactoryConfig) {
			config.Content = SenderContentFactoryFunc(func() (*contentflow.SenderService, error) {
				return nil, io.ErrUnexpectedEOF
			})
		}},
		{"nil content", func(config *SenderFactoryConfig) {
			config.Content = SenderContentFactoryFunc(func() (*contentflow.SenderService, error) {
				return nil, nil
			})
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			factory, err := NewSenderFactory(config)
			if err != nil {
				t.Fatal(err)
			}
			senderChannel, receiverChannel := newMemoryChannelPair()
			sendValidClientHello(t, receiverChannel, factory, byte(index+81))
			_, err = factory.Accept(context.Background(), senderChannel)
			if err == nil {
				t.Fatal("sender dependency failure was accepted")
			}
			_ = senderChannel.Close()
			_ = receiverChannel.Close()
		})
	}
}

func senderConfigFromFixture(fixture *verticalFixture) SenderFactoryConfig {
	return SenderFactoryConfig{
		ShareInstance:    fixture.senderFactory.share,
		SessionAuthKey:   bytes.Clone(fixture.senderFactory.authKey),
		SenderPrivateKey: bytes.Clone(fixture.senderFactory.privateKey),
		Catalog:          fixture.senderFactory.catalog,
		Content:          fixture.senderFactory.content,
		Peers:            fixture.senderFactory.peers,
		Random:           &deterministicReader{next: 101},
		InitialLaneIDs: InitialLaneIDSourceFunc(func() (uint32, error) {
			return 101, nil
		}),
		TerminalConnectivity: TerminalConnectivityFuncs{CleanupFunc: func(context.Context) error { return nil }},
		TerminalTimeout:      time.Second,
	}
}

func sendValidClientHello(t *testing.T, channel *memoryChannel, factory *SenderFactory, seed byte) {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(&deterministicReader{next: seed})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := protocolsession.NewClientHello(
		factory.share,
		id16[protocolsession.ReceiverInstanceID](seed),
		bytes.Repeat([]byte{seed + 1}, protocolsession.HandshakeNonceBytes),
		private.PublicKey(),
		factory.authKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Send(context.Background(), framechannel.Frame(hello.Encoded())); err != nil {
		t.Fatal(err)
	}
}

type edgeErrorReader struct{}

func (edgeErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
