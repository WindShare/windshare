package sendersession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/receiversession"
	"testing"
	"time"
)

func TestSessionControlsRouteWithoutNegotiation(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	// A wrapper must retain path controls through the public contract.
	senderSession := struct{ PeerSession }{
		PeerSession: senderPeerSession{runtime: sender, outbound: sender.outbound},
	}
	type receivedControl struct {
		kind protocolsession.MessageKind
		body []byte
	}
	senderReceived := make(chan receivedControl, 4)
	receiverReceived := make(chan receivedControl, 4)
	receive := func(destination chan<- receivedControl, kind protocolsession.MessageKind) func(context.Context, []byte) error {
		return func(_ context.Context, body []byte) error {
			destination <- receivedControl{kind, body}
			return nil
		}
	}
	senderSession.SetPeerPathControlHandler(receive(senderReceived, protocolsession.MessagePeerPathControl))
	receiver.SetPeerPathControlHandler(receive(receiverReceived, protocolsession.MessagePeerPathControl))
	body, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1, Kind: protocolsession.PeerPathDemand, ValidFor: time.Minute, HoldFor: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assertDelivery := func(send func(context.Context, []byte) error, received <-chan receivedControl, want receivedControl) {
		t.Helper()
		if err := send(ctx, want.body); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-received:
			if got.kind != want.kind || !bytes.Equal(got.body, want.body) {
				t.Fatalf("control reached wrong consumer: kind=%d want=%d", got.kind, want.kind)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	path := receivedControl{protocolsession.MessagePeerPathControl, body}
	assertDelivery(receiver.SendPeerPathControl, senderReceived, path)
	assertDelivery(senderSession.SendPeerPathControl, receiverReceived, path)
	receiver.SetPeerPathControlHandler(nil)
	senderSession.SetPeerPathControlHandler(nil)
	if err := receiver.SendPeerPathControl(ctx, []byte{1}); err == nil {
		t.Fatal("invalid outbound body")
	}
	if err := senderSession.SendPeerPathControl(ctx, []byte{1}); err == nil {
		t.Fatal("invalid signed outbound body")
	}
	if err := (*receiversession.Runtime)(nil).SendPeerPathControl(ctx, body); err == nil {
		t.Fatal("nil receiver accepted control")
	}
	if err := (senderPeerSession{}).SendPeerPathControl(ctx, body); err == nil {
		t.Fatal("nil sender accepted control")
	}
}
