package connectivity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
)

const orchestrationTestTimeout = 3 * time.Second

func TestSenderServesRelayAndPeerWithSharedDependencies(t *testing.T) {
	relayChannel := newFakePeerChannel()
	peerChannel := newFakePeerChannel()
	store := &recordingStore{block: []byte("shared-source")}
	sealer := &recordingSealer{}
	sender, err := NewSender(answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return peerChannel, nil
	}), SenderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- sender.ServeReceiver(t.Context(), relayChannel, &inertSignaling{}, store, sealer)
	}()
	request, _ := session.EncodeRequest([]uint64{0})
	if err := relayChannel.deliver(request); err != nil {
		t.Fatal(err)
	}
	if err := peerChannel.deliver(request); err != nil {
		t.Fatal(err)
	}
	assertBlockSent(t, relayChannel)
	assertBlockSent(t, peerChannel)
	relayChannel.closeWithError(nil)
	peerChannel.closeWithError(nil)
	if err := waitError(t, result); err != nil {
		t.Fatalf("ServeReceiver: %v", err)
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("shared store calls = %d, want 2", got)
	}
	if got := sealer.calls.Load(); got != 2 {
		t.Fatalf("shared sealer calls = %d, want 2", got)
	}
}

func TestSenderKeepsRelayWhenPeerNegotiationFails(t *testing.T) {
	wantPeerErr := errors.New("peer negotiation unavailable")
	relayChannel := newFakePeerChannel()
	store := &recordingStore{block: []byte("relay-fallback")}
	sealer := &recordingSealer{}
	reported := make(chan error, 1)
	sender, err := NewSender(answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return nil, wantPeerErr
	}), SenderOptions{OnPathError: func(path Path, err error) {
		if path != PeerPath {
			t.Errorf("reported path = %q", path)
		}
		reported <- err
	}})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- sender.ServeReceiver(t.Context(), relayChannel, &inertSignaling{}, store, sealer)
	}()
	request, _ := session.EncodeRequest([]uint64{0})
	if err := relayChannel.deliver(request); err != nil {
		t.Fatal(err)
	}
	assertBlockSent(t, relayChannel)
	if err := <-reported; !errors.Is(err, wantPeerErr) {
		t.Fatalf("reported error = %v", err)
	}
	relayChannel.closeWithError(nil)
	if err := waitError(t, result); err != nil {
		t.Fatalf("relay fallback ended with %v", err)
	}
}

func TestSenderFatalSourceFailureCancelsSiblingAfterTerminal(t *testing.T) {
	wantErr := errors.New("source drift")
	relayChannel := newFakePeerChannel()
	peerChannel := newFakePeerChannel()
	store := &recordingStore{err: wantErr}
	sealer := &recordingSealer{}
	sender, err := NewSender(answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return peerChannel, nil
	}), SenderOptions{ClassifySessionError: func(err error) SendErrorDisposition {
		if errors.Is(err, wantErr) {
			return SendShareFatal
		}
		return SendPathEnded
	}})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- sender.ServeReceiver(t.Context(), relayChannel, &inertSignaling{}, store, sealer)
	}()
	request, _ := session.EncodeRequest([]uint64{0})
	if err := relayChannel.deliver(request); err != nil {
		t.Fatal(err)
	}
	select {
	case sent := <-relayChannel.sent:
		if !sent.terminal {
			t.Fatal("source failure was not delivered through SendTerminal")
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("timed out waiting for terminal failure")
	}
	if err := waitError(t, result); !errors.Is(err, wantErr) {
		t.Fatalf("ServeReceiver error = %v", err)
	}
	select {
	case <-peerChannel.done:
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("fatal source failure did not cancel the sibling path")
	}
}

func TestSenderSlowReceiverDoesNotBlockSibling(t *testing.T) {
	slowRelay := newFakePeerChannel()
	healthyRelay := newFakePeerChannel()
	slowGate := slowRelay.blockSends()
	slowPeer := newFakePeerChannel()
	healthyPeer := newFakePeerChannel()
	slowPeer.closeWithError(nil)
	healthyPeer.closeWithError(nil)
	peers := map[string]PeerChannel{"slow": slowPeer, "healthy": healthyPeer}
	sender, err := NewSender(answerFactoryFunc(func(_ context.Context, signaling Signaling) (PeerChannel, error) {
		return peers[signaling.(*inertSignaling).id], nil
	}), SenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{block: []byte("fan-out")}
	sealer := &recordingSealer{}
	slowCtx, cancelSlow := context.WithCancel(context.Background())
	healthyCtx, cancelHealthy := context.WithCancel(context.Background())
	defer cancelSlow()
	defer cancelHealthy()
	slowResult := make(chan error, 1)
	healthyResult := make(chan error, 1)
	go func() {
		slowResult <- sender.ServeReceiver(slowCtx, slowRelay, &inertSignaling{id: "slow"}, store, sealer)
	}()
	go func() {
		healthyResult <- sender.ServeReceiver(healthyCtx, healthyRelay, &inertSignaling{id: "healthy"}, store, sealer)
	}()
	request, _ := session.EncodeRequest([]uint64{0})
	_ = slowRelay.deliver(request)
	_ = healthyRelay.deliver(request)
	assertBlockSent(t, healthyRelay)
	select {
	case <-slowRelay.sent:
		t.Fatal("slow receiver unexpectedly passed its blocked transport")
	default:
	}
	cancelSlow()
	cancelHealthy()
	close(slowGate)
	if err := waitError(t, slowResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("slow result = %v", err)
	}
	if err := waitError(t, healthyResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("healthy result = %v", err)
	}
}

func TestNewSenderRejectsMissingFactory(t *testing.T) {
	if _, err := NewSender(nil, SenderOptions{}); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("NewSender error = %v", err)
	}
}

func TestSenderRejectsNilChannelFromFactory(t *testing.T) {
	sender, err := NewSender(answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return nil, nil
	}), SenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.ServeReceiver(
		t.Context(),
		newFakePeerChannel(),
		&inertSignaling{},
		&recordingStore{block: []byte("unused")},
		&recordingSealer{},
	)
	if !errors.Is(err, ErrNilDependency) {
		t.Fatalf("ServeReceiver error = %v", err)
	}
}

func assertBlockSent(t *testing.T, channel *fakePeerChannel) {
	t.Helper()
	select {
	case sent := <-channel.sent:
		message, err := session.Decode(sent.frame)
		if err != nil {
			t.Fatal(err)
		}
		if sent.terminal {
			t.Fatal("ordinary block was marked terminal")
		}
		if _, ok := message.(*session.Block); !ok {
			t.Fatalf("sent message type = %T", message)
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("timed out waiting for block")
	}
}

func waitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("timed out waiting for result")
		return nil
	}
}
