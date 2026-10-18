package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const transferFrames = 80
const maximumEnvelopeBytes = v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes

// This covers many feedback round trips on a remote Preview. Individual I/O
// operations retain their short deadline; local scenarios finish immediately.
const deliveryTimeout = 30 * time.Second

func newDeliveryFixture() fixture {
	f := newFixture()
	f.descriptor = bytes.Repeat([]byte{0xa5}, v2.MaxDescriptorBytes)
	f.init.Registration.DescriptorDigest = sha256.Sum256(f.descriptor)
	return f
}

type deliveryPeer struct {
	connection *websocket.Conn
	session    v2.RelaySessionID
	side       byte
	capacity   v2.ReceiveCapacity
	granted    v2.DeliveryConsumed
	sent       v2.DeliveryConsumed
	received   v2.DeliveryConsumed
	consumed   v2.DeliveryConsumed
	settled    bool
	closed     bool
	probeNonce uint64
	probeAck   uint64
}

type deliveryEvent struct {
	peer  *deliveryPeer
	bytes []byte
	err   error
}

type deliverySession struct {
	peers          [2]*deliveryPeer
	events         chan deliveryEvent
	cancel         context.CancelFunc
	consumeThrough uint64
}

// Both directions exceed every declared window. Alternating tiny and maximum
// envelopes detects implementations that replenish only one resource dimension.
func transfer(endpoint string, f fixture, sender *websocket.Conn) *deliverySession {
	session := beginDelivery(endpoint, f, sender)
	session.through(transferFrames + 1)
	return session
}

func beginDelivery(endpoint string, f fixture, sender *websocket.Conn) *deliverySession {
	receiver, _ := dial(endpoint, f.init.Registration.ShareID)
	capacity := v2.ReceiveCapacity{Frames: 4, EncodedBytes: 2 * maximumEnvelopeBytes}
	send(receiver, v2.DeliveryJoin{ShareID: f.init.Registration.ShareID, ReceiveCapacity: capacity})
	descriptor, err := v2.ParseDescriptorDelivery(read(receiver))
	check(err)
	require(bytes.Equal(descriptor.Object, f.descriptor), "JOIN descriptor differs from committed bytes")

	ctx, cancel := context.WithCancel(context.Background())
	session := &deliverySession{events: make(chan deliveryEvent, 32), cancel: cancel, consumeThrough: transferFrames}
	for index, connection := range []*websocket.Conn{sender, receiver} {
		peer := &deliveryPeer{
			connection: connection, session: descriptor.RelaySessionID, side: byte(index),
			capacity: f.init.ReceiveCapacity,
		}
		if index == 1 {
			peer.capacity = capacity
		}
		peer.received.RelaySessionID = peer.session
		session.peers[index] = peer
		go session.read(ctx, peer)
	}
	return session
}

func (s *deliverySession) through(target uint64) {
	s.until(func() bool {
		return s.peers[0].received.TotalFrames == target && s.peers[1].received.TotalFrames == target
	}, target)
}

func (s *deliverySession) barrier(nonce uint64) {
	for _, peer := range s.peers {
		peer.probeNonce = nonce
		send(peer.connection, v2.ConnectionProbe{Nonce: nonce})
	}
	s.until(func() bool {
		return s.peers[0].probeAck == nonce && s.peers[1].probeAck == nonce
	}, s.peers[0].sent.TotalFrames)
}

func (s *deliverySession) read(ctx context.Context, peer *deliveryPeer) {
	for {
		kind, frame, err := peer.connection.Read(ctx)
		if err == nil && kind != websocket.MessageBinary {
			err = fmt.Errorf("nonbinary relay output")
		}
		select {
		case s.events <- deliveryEvent{peer: peer, bytes: frame, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func ciphertext(side byte, ordinal uint64) []byte {
	sizes := [...]int{1, v2.MaxOpaqueCiphertextBytes, 8192}
	return bytes.Repeat([]byte{side*97 + byte(ordinal)}, sizes[ordinal%uint64(len(sizes))])
}

func (s *deliverySession) until(done func() bool, target uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()
	for !done() {
		s.pump(target)
		select {
		case event := <-s.events:
			check(event.err)
			s.accept(event, false)
		case <-ctx.Done():
			panic(fmt.Sprintf("delivery stalled beyond initial window: sender=%+v receiver=%+v",
				s.peers[0], s.peers[1]))
		}
	}
}

func (s *deliverySession) pump(target uint64) {
	for _, peer := range s.peers {
		for peer.sent.TotalFrames < target {
			payload := ciphertext(peer.side, peer.sent.TotalFrames)
			cost := uint64(v2.OpaqueRouteHeaderBytes + len(payload))
			if peer.granted.TotalFrames == peer.sent.TotalFrames ||
				peer.granted.TotalEncodedBytes-peer.sent.TotalEncodedBytes < cost {
				break
			}
			send(peer.connection, v2.OpaqueRoute{RelaySessionID: peer.session, Ciphertext: payload})
			peer.sent.TotalFrames++
			peer.sent.TotalEncodedBytes += cost
		}
	}
}

func (peer *deliveryPeer) consume() {
	peer.consumed = peer.received
	send(peer.connection, peer.consumed)
	// Exact replays are legal but must never mint another allowance.
	send(peer.connection, peer.consumed)
}

func (s *deliverySession) accept(event deliveryEvent, stopping bool) {
	peer := event.peer
	require(len(event.bytes) >= 4, "short relay output")
	switch string(event.bytes[:4]) {
	case "WS2A":
		ack, err := v2.ParseConnectionProbeAck(event.bytes)
		check(err)
		require(ack.Nonce == peer.probeNonce, "delivery heartbeat nonce mismatch")
		peer.probeAck = ack.Nonce
	case v2.SessionCreditMagic:
		grant, err := v2.ParseSessionCredit(event.bytes)
		check(err)
		require(grant.RelaySessionID == peer.session, "credit belongs to another session")
		peer.granted.TotalFrames += uint64(grant.Frames)
		peer.granted.TotalEncodedBytes += uint64(grant.Bytes)
		destination := s.peers[1-int(peer.side)]
		consumed := destination.consumed
		require(peer.granted.TotalFrames-consumed.TotalFrames <= destination.capacity.Frames &&
			peer.granted.TotalEncodedBytes-consumed.TotalEncodedBytes <= destination.capacity.EncodedBytes,
			"feedback replay over-granted destination capacity")
	case "WS2O":
		require(!peer.settled, "opaque payload followed its frozen settlement")
		frame, err := v2.ParseOpaqueRoute(event.bytes)
		check(err)
		require(frame.RelaySessionID == peer.session, "payload belongs to another session")
		require(bytes.Equal(frame.Ciphertext, ciphertext(1-peer.side, peer.received.TotalFrames)),
			"opaque payload changed or arrived out of order")
		if peer.side == 0 && peer.received.TotalFrames == 0 {
			send(peer.connection, v2.SessionAdmitted{RelaySessionID: peer.session})
		}
		peer.received.TotalFrames++
		peer.received.TotalEncodedBytes += uint64(len(event.bytes))
		if peer.received.TotalFrames <= s.consumeThrough && peer.received.TotalFrames%2 == 0 {
			peer.consume()
		}
	case v2.SettlementNoticeMagic:
		require(stopping, "session retired during transfer")
		notice, err := v2.ParseSettlementNotice(event.bytes)
		check(err)
		require(notice.RelaySessionID == peer.session &&
			notice.TotalFrames == peer.received.TotalFrames &&
			notice.TotalEncodedBytes == peer.received.TotalEncodedBytes,
			"settlement did not follow its exact submitted prefix")
		peer.settled = true
	default:
		panic(fmt.Sprintf("unexpected delivery output %q", event.bytes[:4]))
	}
}

func (s *deliverySession) stopped() {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for !s.peers[0].closed || !s.peers[1].closed {
		select {
		case event := <-s.events:
			if event.err != nil {
				require(event.peer.settled, "STOP closed a destination before its ordered settlement")
				event.peer.closed = true
			} else {
				s.accept(event, true)
			}
		case <-ctx.Done():
			panic("STOP did not close both delivery connections")
		}
	}
}

func (s *deliverySession) close() {
	s.cancel()
	_ = s.peers[1].connection.CloseNow()
}
