package receiversession

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeDependencyAdaptersAndStableErrors(t *testing.T) {
	var stopped, cleaned atomic.Int32
	connectivity := sendersession.TerminalConnectivityFuncs{
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
	sendersession.TerminalConnectivityFuncs{}.StopRecovery()
	if err := (sendersession.TerminalConnectivityFuncs{}).Cleanup(context.Background()); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil cleanup error = %v", err)
	}
	if _, err := (sendersession.ContentFactoryFunc(nil)).NewSenderContentService(id16[protocolsession.ProtocolSessionID](1)); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil content factory error = %v", err)
	}
	if _, err := (sendersession.InitialLaneIDSourceFunc(nil)).NextInitialLaneID(); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil lane source error = %v", err)
	}
	if _, err := (InstanceSourceFunc(nil)).NewReceiverInstanceID(); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
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
		(&sessioncontract.LaneRejectedError{}).Error() == "" {
		t.Fatal("typed remote errors lost stable diagnostics")
	}

	if (*Runtime)(nil).DetachLane(sessioncontract.LaneIdentity{}) || (*sendersession.Runtime)(nil).DetachLane(sessioncontract.LaneIdentity{}) ||
		(*Runtime)(nil).AttachedLanes() != 0 || (*sendersession.Runtime)(nil).AttachedLanes() != 0 {
		t.Fatal("nil lane accessors were not inert")
	}
	(*Runtime)(nil).Close()
	(*sendersession.Runtime)(nil).Close()
	if err := (*sendersession.Runtime)(nil).Stop(context.Background(), "stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("nil sender stop error = %v", err)
	}
	if err := (*sendersession.Factory)(nil).Stop(context.Background(), "stop"); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
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
	factory, err := sendersession.NewFactory(defaulted)
	if err != nil {
		t.Fatal(err)
	}
	if err := factory.Stop(context.Background(), "unused factory"); err != nil {
		t.Fatal(err)
	}
	if err := factory.Stop(context.Background(), "repeat"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*sendersession.FactoryConfig){
		"zero share":      func(config *sendersession.FactoryConfig) { config.ShareInstance = catalog.ShareInstance{} },
		"missing auth":    func(config *sendersession.FactoryConfig) { config.SessionAuthKey = nil },
		"missing key":     func(config *sendersession.FactoryConfig) { config.SenderPrivateKey = nil },
		"missing catalog": func(config *sendersession.FactoryConfig) { config.Catalog = nil },
		"missing content": func(config *sendersession.FactoryConfig) { config.Content = nil },
		"missing cleanup": func(config *sendersession.FactoryConfig) { config.TerminalConnectivity = nil },
		"negative timeout": func(config *sendersession.FactoryConfig) {
			config.TerminalTimeout = -time.Second
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := baseSender
			mutate(&config)
			if _, err := sendersession.NewFactory(config); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
				t.Fatalf("factory error = %v", err)
			}
		})
	}

	receiverDefaults := fixture.receiverConfig
	receiverDefaults.Random = nil
	receiverDefaults.ReceiverInstances = nil
	receiverDefaults.After = nil
	if _, err := NewFactory(receiverDefaults); err != nil {
		t.Fatalf("receiver defaults: %v", err)
	}
	badReceiver := fixture.receiverConfig
	badReceiver.SessionAuthKey = nil
	if _, err := NewFactory(badReceiver); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("invalid receiver error = %v", err)
	}
	if _, err := (*Factory)(nil).Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil receiver connect error = %v", err)
	}
	if _, err := fixture.receiverFactory.Connect(context.Background(), nil, transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil receiver channel error = %v", err)
	}
	if _, err := (*sendersession.Factory)(nil).Accept(context.Background(), newMemoryChannel(t)); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil sender accept error = %v", err)
	}
	if _, err := fixture.senderFactory.Accept(context.Background(), nil); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil sender channel error = %v", err)
	}

	randomFailure := fixture.receiverConfig
	randomFailure.Random = edgeErrorReader{}
	randomFactory, _ := NewFactory(randomFailure)
	if _, err := randomFactory.Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("receiver random error = %v", err)
	}
	instanceFailure := fixture.receiverConfig
	instanceFailure.ReceiverInstances = InstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
		return protocolsession.ReceiverInstanceID{}, io.ErrUnexpectedEOF
	})
	instanceFactory, _ := NewFactory(instanceFailure)
	if _, err := instanceFactory.Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("receiver identity error = %v", err)
	}
	zeroInstance := fixture.receiverConfig
	zeroInstance.ReceiverInstances = InstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
		return protocolsession.ReceiverInstanceID{}, nil
	})
	zeroFactory, _ := NewFactory(zeroInstance)
	if _, err := zeroFactory.Connect(context.Background(), newMemoryChannel(t), transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("zero receiver identity error = %v", err)
	}

	closed, closedPeer := newMemoryChannelPair()
	_ = closed.Close()
	t.Cleanup(func() { _ = closedPeer.Close() })
	if _, err := fixture.receiverFactory.Connect(context.Background(), closed, transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("receiver send error = %v", err)
	}
	invalidServer, invalidClient := newMemoryChannelPair()
	go func() {
		<-invalidServer.Recv()
		_ = invalidServer.Send(context.Background(), framechannel.Frame{1})
	}()
	if _, err := fixture.receiverFactory.Connect(context.Background(), invalidClient, transferlanes.RouteRelay); !errors.Is(err, sessioncontract.ErrHandshake) {
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
	if _, err := fixture.senderFactory.Accept(context.Background(), malformedSender); !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("malformed client hello error = %v", err)
	}
	_ = malformedSender.Close()
	_ = malformedPeer.Close()

	testSenderAcceptDependencyFailures(t, baseSender)
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
	if err := receiver.revisions.leaseError(unknownLease); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("unknown lease state error = %v", err)
	}
	first, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); err == nil {
		t.Fatal("duplicate lease was accepted")
	} else {
		if _, ok := errors.AsType[RemoteOperationError](err); !ok {
			t.Fatalf("duplicate lease error = %v", err)
		}
	}
	if err := receiver.ReleaseRevision(context.Background(), first.LeaseID); err != nil {
		t.Fatal(err)
	}
}

func testSenderAcceptDependencyFailures(t *testing.T, base sendersession.FactoryConfig) {
	t.Helper()
	tests := []struct {
		name   string
		mutate func(*sendersession.FactoryConfig)
	}{
		{"random", func(config *sendersession.FactoryConfig) { config.Random = edgeErrorReader{} }},
		{"lane id", func(config *sendersession.FactoryConfig) {
			config.InitialLaneIDs = sendersession.InitialLaneIDSourceFunc(func() (uint32, error) { return 0, io.ErrUnexpectedEOF })
		}},
		{"zero lane id", func(config *sendersession.FactoryConfig) {
			config.InitialLaneIDs = sendersession.InitialLaneIDSourceFunc(func() (uint32, error) { return 0, nil })
		}},
		{"catalog", func(config *sendersession.FactoryConfig) {
			config.Catalog = sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return nil, io.ErrUnexpectedEOF
			})
		}},
		{"nil catalog", func(config *sendersession.FactoryConfig) {
			config.Catalog = sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return nil, nil
			})
		}},
		{"content", func(config *sendersession.FactoryConfig) {
			config.Content = sendersession.ContentFactoryFunc(func(protocolsession.ProtocolSessionID) (*contentflow.SenderService, error) {
				return nil, io.ErrUnexpectedEOF
			})
		}},
		{"nil content", func(config *sendersession.FactoryConfig) {
			config.Content = sendersession.ContentFactoryFunc(func(protocolsession.ProtocolSessionID) (*contentflow.SenderService, error) {
				return nil, nil
			})
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			factory, err := sendersession.NewFactory(config)
			if err != nil {
				t.Fatal(err)
			}
			senderChannel, receiverChannel := newMemoryChannelPair()
			sendValidClientHello(t, receiverChannel, config, byte(index+81))
			_, err = factory.Accept(context.Background(), senderChannel)
			if err == nil {
				t.Fatal("sender dependency failure was accepted")
			}
			_ = senderChannel.Close()
			_ = receiverChannel.Close()
		})
	}
}

func senderConfigFromFixture(fixture *verticalFixture) sendersession.FactoryConfig {
	return sendersession.FactoryConfig{
		ShareInstance:    fixture.senderConfig.ShareInstance,
		SessionAuthKey:   bytes.Clone(fixture.senderConfig.SessionAuthKey),
		SenderPrivateKey: bytes.Clone(fixture.senderConfig.SenderPrivateKey),
		Catalog:          fixture.senderConfig.Catalog,
		Content:          fixture.senderConfig.Content,
		Peers:            fixture.senderConfig.Peers,
		Random:           &deterministicReader{next: 101},
		InitialLaneIDs: sendersession.InitialLaneIDSourceFunc(func() (uint32, error) {
			return 101, nil
		}),
		TerminalConnectivity: sendersession.TerminalConnectivityFuncs{CleanupFunc: func(context.Context) error { return nil }},
		TerminalTimeout:      time.Second,
	}
}

func sendValidClientHello(t *testing.T, channel *memoryChannel, config sendersession.FactoryConfig, seed byte) {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(&deterministicReader{next: seed})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := protocolsession.NewClientHello(
		config.ShareInstance,
		id16[protocolsession.ReceiverInstanceID](seed),
		bytes.Repeat([]byte{seed + 1}, protocolsession.HandshakeNonceBytes),
		private.PublicKey(),
		config.SessionAuthKey,
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
