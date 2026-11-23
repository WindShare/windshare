package v2peer_test

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/peerslot"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

type wireDescriptions struct {
	receiverIntegrationSignaling
	offer, candidates chan []byte
}

func (s wireDescriptions) OpenPeerOperation(ctx context.Context, binding v2peer.ReceiverSignalingOperationBinding, offer []byte) (v2peer.ReceiverSignalingOperation, error) {
	s.offer <- offer
	op, err := s.receiverIntegrationSignaling.OpenPeerOperation(ctx, binding, offer)
	if err != nil {
		return nil, err
	}
	return wireCandidates{op, s.candidates}, nil
}

type wireCandidates struct {
	v2peer.ReceiverSignalingOperation
	sent chan []byte
}

func (w wireCandidates) SendCandidate(_ context.Context, b []byte) (p.OperationDisposition, error) {
	w.sent <- b
	return p.OperationDeliver, nil
}

type wireAnswer struct {
	*receiverIntegrationSenderSession
	answer, candidates chan []byte
}

func (s wireAnswer) SendPeerControl(_ context.Context, _ p.Operation, k p.MessageKind, b []byte) (p.OperationDisposition, error) {
	switch k {
	case p.MessagePeerAnswer:
		s.answer <- b
	case p.MessagePeerCandidate:
		s.candidates <- b
	}
	return p.OperationDeliver, nil
}

// Force native gathering to complete before v2peer snapshots the SDP. This
// makes Pion include its own end marker while leased-port STUN is still pending.
type gatheredPeer struct {
	*pionpeer.Connection
	ctx context.Context
}

func (p gatheredPeer) SetLocalDescription(s pion.SessionDescription) error {
	done := pion.GatheringCompletePromise(p.PeerConnection)
	if err := p.Connection.SetLocalDescription(s); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func TestActualPeerSignalsRemainOpenForLateHostPortSTUN(t *testing.T) {
	requireLongPionIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// Select the host interface without sending a packet outside the machine.
	probe, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9})
	if err != nil {
		t.Skip("integration requires a local IPv4 interface")
	}
	local := probe.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	probe.Close()
	server, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	config, err := iceconfig.New([]string{"stun:" + server.LocalAddr().String()})
	if err != nil {
		t.Fatal(err)
	}
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	newPeer := func(id byte, base pion.Configuration) (gatheredPeer, error) {
		lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{id}, []netip.Addr{local})
		if err != nil {
			return gatheredPeer{}, err
		}
		defer lease.Close()
		conn, err := pionpeer.NewPeerConnection(base, pionpeer.AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease, ICE: config})
		return gatheredPeer{conn, ctx}, err
	}
	op := newReceiverIntegrationOperation(integrationOperationID(73))
	sessionID := p.ProtocolSessionID{2}
	lane := session.LaneIdentity{ID: 2, Epoch: 1}
	answer := wireAnswer{&receiverIntegrationSenderSession{share: catalog.ShareInstance{1}, sessionID: sessionID, operation: op, lane: lane, channels: make(chan p.FrameChannel, 1)}, make(chan []byte, 1), make(chan []byte, 32)}
	send, err := v2peer.NewFactory(v2peer.Config{Native: newCapacityNative(t), MaxCandidates: v2PeerIntegrationMaxCandidates,
		PeerConnections: v2peer.PeerConnectionFactoryFunc(func(base pion.Configuration) (v2peer.PeerConnection, error) { return newPeer(1, base) })})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := send.NewSenderPeerHandler(answer)
	if err != nil {
		t.Fatal(err)
	}
	op.ingress, err = newIntegrationPeerIngress(send, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer op.ingress.Close()
	ended := make(chan struct{})
	go func() { defer close(ended); _ = handler.Run(ctx) }()
	defer func() { cancel(); <-ended }()
	recv, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{Native: newCapacityNative(t), MaxCandidates: v2PeerIntegrationMaxCandidates,
		PeerConnections: v2peer.ReceiverPeerConnectionFactoryFunc(func(base pion.Configuration) (v2peer.ReceiverPeerConnection, error) { return newPeer(2, base) })})
	if err != nil {
		t.Fatal(err)
	}
	offer := wireDescriptions{receiverIntegrationSignaling{op}, make(chan []byte, 1), make(chan []byte, 32)}
	attempt, err := recv.Start(ctx, offer, receiverIntegrationLanes{lane: lane, channels: make(chan p.FrameChannel, 1)}, integrationPeerSlot(t, &peerslot.Pool{}))
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	read := func(c <-chan []byte) []byte {
		t.Helper()
		select {
		case value := <-c:
			return value
		case <-ctx.Done():
			t.Fatal("production signal timed out")
			return nil
		}
	}
	o, err := v2signal.DecodeOffer(read(offer.offer))
	if err != nil || strings.Contains(o.SDP, "a=end-of-candidates") {
		t.Fatal("offer closed trickle gathering", err)
	}
	a, err := v2signal.DecodeAnswer(read(answer.answer))
	if err != nil || strings.Contains(a.SDP, "a=end-of-candidates") {
		t.Fatal("answer closed trickle gathering", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
	answered := make(map[netip.AddrPort]bool)
	for len(answered) < 2 {
		var buf [1500]byte
		n, source, err := server.ReadFromUDPAddrPort(buf[:])
		if err != nil {
			t.Fatal(err)
		}
		msg := &stun.Message{Raw: buf[:n]}
		if msg.Decode() != nil || msg.Type != stun.BindingRequest {
			continue
		}
		reply, err := stun.Build(stun.NewTransactionIDSetter(msg.TransactionID), stun.BindingSuccess, &stun.XORMappedAddress{IP: source.Addr().AsSlice(), Port: int(source.Port())})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.WriteToUDPAddrPort(reply.Raw, source); err != nil {
			t.Fatal(err)
		}
		answered[source] = true
	}
	for _, received := range []chan []byte{offer.candidates, answer.candidates} {
		for {
			candidate, err := v2signal.DecodeCandidate(read(received))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(candidate.Candidate, " typ srflx ") {
				break
			}
		}
	}
}
