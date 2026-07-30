package cli

import (
	"io"
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
)

func TestSenderPeerFactoryUsesProductionProviderForEveryZeroValueApp(t *testing.T) {
	for _, name := range []string{
		"WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE",
		"WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION",
		"WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256",
		"WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256",
	} {
		t.Setenv(name, "must-not-be-read")
	}
	app := &App{Stderr: io.Discard}
	factory, err := app.newSenderPeerFactory()
	if err != nil || factory == nil {
		t.Fatalf("production sender factory = %v, %v", factory, err)
	}
	if _, ok := any(productionSenderPeerFactoryProvider{}).(SenderPeerFactoryProvider); !ok {
		t.Fatal("production provider no longer satisfies the consumer seam")
	}
}

func TestSenderPeerFactoryPassesObserverAndDiagnosticsToInjectedProvider(t *testing.T) {
	var captured SenderPeerFactoryOptions
	wanted, err := v2peer.NewFactory(v2peer.Config{})
	if err != nil {
		t.Fatalf("test factory: %v", err)
	}
	app := &App{
		Stderr:             io.Discard,
		senderPeerEvidence: io.Discard,
		senderPeerFactories: SenderPeerFactoryProviderFunc(func(options SenderPeerFactoryOptions) (*v2peer.Factory, error) {
			captured = options
			return wanted, nil
		}),
	}
	actual, err := app.newSenderPeerFactory()
	if err != nil || actual != wanted {
		t.Fatalf("injected factory = %v, %v", actual, err)
	}
	if captured.Observer == nil || captured.OnError == nil {
		t.Fatalf("provider options = %#v", captured)
	}
	if _, err := (SenderPeerFactoryProviderFunc(nil)).NewSenderPeerFactory(SenderPeerFactoryOptions{}); err == nil {
		t.Fatal("nil provider function was accepted")
	}
}
