package sendersession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/session/turncontrol"
	"testing"
	"time"
)

func TestSessionControlsRouteIndependentlyWithoutNegotiation(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	// A wrapper must retain both control families through the public contract.
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
	senderSession.SetTURNControlHandler(receive(senderReceived, protocolsession.MessageTURNControl))
	receiver.SetTURNControlHandler(receive(receiverReceived, protocolsession.MessageTURNControl))
	body, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1, Kind: protocolsession.PeerPathDemand, ValidFor: time.Minute, HoldFor: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	turnBody, err := turncontrol.Encode(turncontrol.Available{
		Authority: turncontrol.Header{SessionID: [16]byte(sender.ProtocolSessionID()), OwnerPathID: [16]byte{3}, Sequence: 1},
		Profiles:  []string{"primary"},
	})
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
	turn := receivedControl{protocolsession.MessageTURNControl, turnBody}
	assertDelivery(receiver.SendPeerPathControl, senderReceived, path)
	assertDelivery(senderSession.SendPeerPathControl, receiverReceived, path)
	assertDelivery(receiver.SendTURNControl, senderReceived, turn)
	assertDelivery(senderSession.SendTURNControl, receiverReceived, turn)
	receiver.SetPeerPathControlHandler(nil)
	senderSession.SetPeerPathControlHandler(nil)
	assertDelivery(receiver.SendTURNControl, senderReceived, turn)
	assertDelivery(senderSession.SendTURNControl, receiverReceived, turn)
	receiver.SetTURNControlHandler(nil)
	senderSession.SetTURNControlHandler(nil)
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
	for _, send := range []func(context.Context, []byte) error{receiver.SendTURNControl, senderSession.SendTURNControl} {
		for _, wrongBody := range [][]byte{body, {1}} {
			if err := send(ctx, wrongBody); err == nil {
				t.Fatal("TURN accepted an invalid or reachability body")
			}
		}
		if err := send(nil, turnBody); err == nil {
			t.Fatal("TURN accepted nil context")
		}
	}
	for _, send := range []func(context.Context, []byte) error{receiver.SendPeerPathControl, senderSession.SendPeerPathControl} {
		if err := send(ctx, turnBody); err == nil {
			t.Fatal("reachability accepted TURN body")
		}
	}
	if err := (*receiversession.Runtime)(nil).SendTURNControl(ctx, turnBody); err == nil {
		t.Fatal("nil receiver accepted TURN control")
	}
	if err := (senderPeerSession{}).SendTURNControl(ctx, turnBody); err == nil {
		t.Fatal("nil sender accepted TURN control")
	}
}
