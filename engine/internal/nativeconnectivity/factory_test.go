package nativeconnectivity

import (
	"bytes"
	"context"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/nativepeer"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestSenderObservationModesKeepOrdinaryTransfersQuiet(t *testing.T) {
	for _, config := range []SenderConfig{
		{},
		{TraceLifecycle: true},
		{Diagnostics: true},
		{TraceLifecycle: true, Diagnostics: true},
	} {
		factory, err := NewSender(config)
		if err != nil {
			t.Fatal(err)
		}
		if (factory.SenderAttemptObservations() != nil) != (config.Diagnostics || config.TraceLifecycle) ||
			(factory.PeerDiagnostics() != nil) != config.Diagnostics ||
			(factory.NativeConnectivity().Observations() != nil) != config.Diagnostics {
			t.Fatalf("observation mode differs from owner: %+v", config)
		}
		if err := factory.NativeConnectivity().Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		factory.CompleteObservations()
		factory.NativeConnectivity().CompleteObservations()
	}
	at := time.Unix(42, 0)
	config := senderConfig(SenderConfig{Now: func() time.Time { return at }})
	defer config.Native.Close(context.Background())
	if config.Now() != at {
		t.Fatal("sender replaced the injected clock")
	}
}

func TestReceiverObservationModesAndInjectedNativeAuthority(t *testing.T) {
	for _, diagnostics := range []bool{false, true} {
		factory, err := NewReceiver(ReceiverConfig{Diagnostics: diagnostics})
		if err != nil {
			t.Fatal(err)
		}
		if (factory.ReceiverTerminationObservations() != nil) != diagnostics ||
			(factory.PeerDiagnostics() != nil) != diagnostics ||
			(factory.NativeConnectivity().Observations() != nil) != diagnostics {
			t.Fatalf("receiver observation mode = %v", diagnostics)
		}
		if err := factory.NativeConnectivity().Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		factory.CompleteObservations()
		factory.NativeConnectivity().CompleteObservations()
	}
	native := nativepeer.New(nativepeer.Config{Side: nativepeer.SideReceiver})
	defer native.Close(context.Background())
	random := bytes.NewReader([]byte{1})
	config := receiverConfig(ReceiverConfig{Native: native, Random: random})
	if config.Native != native || config.Random != random {
		t.Fatal("receiver replaced injected authority")
	}
}

func TestDataChannelAdapterPreservesObservationOwnershipAndFailure(t *testing.T) {
	for _, diagnostics := range []bool{false, true} {
		peer, err := pion.NewPeerConnection(pion.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := peer.CreateDataChannel(wsrtc.ChannelLabel, wsrtc.DefaultDataChannelInit())
		if err != nil {
			_ = peer.Close()
			t.Fatal(err)
		}
		var observed *wsrtc.Channel
		adapter := channelAdapter(diagnostics, func(channel *wsrtc.Channel) { observed = channel })
		wrapped, err := adapter.WrapDataChannel(raw)
		if err != nil {
			_ = peer.Close()
			t.Fatal(err)
		}
		if observed == nil || wrapped != observed || (observed.LifecycleTrace() != nil) != diagnostics {
			t.Fatal("wrapped channel ownership or queue mode differs")
		}
		_ = observed.Close()
		_ = peer.Close()
		observed.CompleteObservations()
	}
	called := false
	if _, err := channelAdapter(true, func(*wsrtc.Channel) { called = true }).WrapDataChannel(nil); err == nil || called {
		t.Fatalf("failed channel published ownership: %v, %v", err, called)
	}
}
