package v2endpoint

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestServerFreshResumeJoinForwardAndStop(t *testing.T) {
	const relayBase = "https://relay.example/team?access=test"
	endpoint, err := v2.NormalizeRelayEndpoint(relayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 4, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 16, Random: &sequenceReader{next: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	var connectionSequence atomic.Uint64
	server, err := New(Config{
		Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity,
		ConnectionIDs: ConnectionIDSourceFunc(func() (v2route.ConnectionID, error) {
			return v2route.ConnectionID(fmt.Sprintf("connection-%d", connectionSequence.Add(1))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	dial := memoryServerDialer(server)
	fixture := newEndpointFixture(t)

	sender, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
		RelayBaseURL: relayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		t.Fatalf("fresh registration: %v", err)
	}
	if stats := sender.RegistrationStats(); stats.BytesSent == 0 || stats.BytesReceived == 0 {
		t.Fatalf("registration stats = %+v", stats)
	}

	receiver := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	firstRelaySession := receiver.Channel().RelaySessionID()
	senderChannel := establishSession(t, sender, receiver, []byte("receiver-to-sender"))
	assertFrame(t, senderChannel.Recv(), "receiver-to-sender")
	if err := senderChannel.Send(context.Background(), []byte("sender-to-receiver")); err != nil {
		t.Fatal(err)
	}
	assertFrame(t, receiver.Channel().Recv(), "sender-to-receiver")

	second := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	secondRelaySession := second.Channel().RelaySessionID()
	if secondRelaySession == firstRelaySession {
		t.Fatal("independent receiver joins reused a RelaySessionID")
	}
	secondChannel := establishSession(t, sender, second, []byte("second-session"))
	assertFrame(t, secondChannel.Recv(), "second-session")
	_ = second.Close()
	_ = receiver.Close()
	<-receiver.Done()
	rejoined := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	if rejoined.Channel().RelaySessionID() == firstRelaySession || rejoined.Channel().RelaySessionID() == secondRelaySession {
		t.Fatal("receiver rejoin revived a retired RelaySessionID")
	}
	if !bytes.Equal(rejoined.Descriptor(), fixture.descriptor) {
		t.Fatal("receiver rejoin changed the registered descriptor")
	}
	rejoinedChannel := establishSession(t, sender, rejoined, []byte("receiver-rejoin"))
	assertFrame(t, rejoinedChannel.Recv(), "receiver-rejoin")
	_ = rejoined.Close()
	_ = sender.Close()

	resume, err := relayv2.ResumeInit(fixture.init)
	if err != nil {
		t.Fatal(err)
	}
	var resumed *relayv2.SenderConnection
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resumed, err = relayv2.DialSender(context.Background(), relayv2.SenderConfig{
			RelayBaseURL: relayBase, Init: resume, SenderPrivateKey: fixture.privateKey,
			ResumeToken: fixture.token, Dial: relayv2.DialOptions{SocketDialer: dial},
		})
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatalf("resume registration: %v", err)
	}
	resumedReceiver := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	resumedChannel := establishSession(t, resumed, resumedReceiver, []byte("after-resume"))
	assertFrame(t, resumedChannel.Recv(), "after-resume")

	var stopID v2.StopID
	for index := range stopID {
		stopID[index] = byte(index + 1)
	}
	if err := relayv2.Stop(context.Background(), relayv2.StopConfig{
		RelayBaseURL: relayBase, ShareID: fixture.init.ShareID, ShareInstance: fixture.init.ShareInstance,
		PKHash: fixture.init.PKHash, StopID: stopID, SenderPrivateKey: fixture.privateKey,
		Dial: relayv2.DialOptions{SocketDialer: dial},
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-resumed.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not close sender")
	}
	if _, err := relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: relayBase, ShareID: fixture.init.ShareID,
		Dial: relayv2.DialOptions{SocketDialer: dial},
	}); err == nil {
		t.Fatal("stopped share remained joinable")
	}
	_ = resumedReceiver.Close()
	_ = resumed.Close()
}

func TestRetiredSessionLateSenderFrameDoesNotKillRejoinedSession(t *testing.T) {
	const relayBase = "https://relay.example/retired-session"
	endpoint, err := v2.NormalizeRelayEndpoint(relayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 4, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 16, Random: &sequenceReader{next: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	var connectionSequence atomic.Uint64
	server, err := New(Config{
		Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity,
		ConnectionIDs: ConnectionIDSourceFunc(func() (v2route.ConnectionID, error) {
			return v2route.ConnectionID(fmt.Sprintf("retirement-%d", connectionSequence.Add(1))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	type serveResult struct{ done <-chan error }
	served := make(chan serveResult, 4)
	dial := func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		done := make(chan error, 1)
		served <- serveResult{done: done}
		go func() { done <- server.Serve(context.Background(), relay) }()
		return client, nil
	}
	fixture := newEndpointFixture(t)
	sender, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
		RelayBaseURL: relayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	senderServe := (<-served).done
	senderRef := endpointCurrentConnectionRef(t, server, "retirement-1")

	receiverA := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	receiverAServe := (<-served).done
	oldSessionID := receiverA.Channel().RelaySessionID()
	oldSenderChannel := establishSession(t, sender, receiverA, []byte("receiver-a"))
	assertFrame(t, oldSenderChannel.Recv(), "receiver-a")
	if err := receiverA.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-receiverAServe:
		if err != nil {
			t.Fatalf("receiver A relay cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver A relay cleanup did not finish")
	}
	retired, err := registry.ResolveSession(oldSessionID, senderRef)
	if err != nil || retired.Disposition != v2route.SessionRetired {
		t.Fatalf("old session retirement = %+v, %v", retired, err)
	}

	receiverB := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	defer receiverB.Close()
	receiverBServe := (<-served).done
	receiverBRef := endpointCurrentConnectionRef(t, server, "retirement-3")
	newSessionID := receiverB.Channel().RelaySessionID()
	if newSessionID == oldSessionID {
		t.Fatal("receiver B revived the retired RelaySessionID")
	}
	newSenderChannel := establishSession(t, sender, receiverB, []byte("receiver-b"))
	assertFrame(t, newSenderChannel.Recv(), "receiver-b")

	select {
	case _, open := <-oldSenderChannel.Recv():
		if open || !errors.Is(oldSenderChannel.Err(), relayv2.ErrSessionRetired) {
			t.Fatalf("retired sender channel open=%t error=%v", open, oldSenderChannel.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not retire the old sender channel")
	}
	if err := oldSenderChannel.Send(context.Background(), []byte("late-retired-frame")); !errors.Is(err, relayv2.ErrClosed) {
		t.Fatalf("late retired send error = %v", err)
	}
	if err := newSenderChannel.Send(context.Background(), []byte("sender-to-b-after-late")); err != nil {
		t.Fatalf("new session send after retired frame: %v", err)
	}
	assertFrame(t, receiverB.Channel().Recv(), "sender-to-b-after-late")
	if err := receiverB.Channel().Send(context.Background(), []byte("b-to-sender-after-late")); err != nil {
		t.Fatalf("new receiver reply after retired frame: %v", err)
	}
	assertFrame(t, newSenderChannel.Recv(), "b-to-sender-after-late")

	active, err := registry.ResolveSession(newSessionID, senderRef)
	if err != nil || active.Disposition != v2route.SessionForward || active.Destination != receiverBRef {
		t.Fatalf("receiver B route after retired frame = %+v, %v", active, err)
	}
	select {
	case <-sender.Done():
		t.Fatal("retired session frame closed the multiplexed sender connection")
	case <-receiverB.Done():
		t.Fatal("retired session frame closed receiver B")
	default:
	}

	if err := receiverB.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-receiverBServe:
		if err != nil {
			t.Fatalf("receiver B relay cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver B relay cleanup did not finish")
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-senderServe:
		if err != nil {
			t.Fatalf("sender relay cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sender relay cleanup did not finish")
	}
}

func TestForwardFrameIsolatesSenderSessionsAndRejectsHostileIDs(t *testing.T) {
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 2, MaxSessions: 8, MaxSessionsPerShare: 4,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	sender := newEndpointTestConnection("sender", nil, func() {})
	receiverA := newEndpointTestConnection("receiver-a", nil, func() {})
	receiverB := newEndpointTestConnection("receiver-b", nil, func() {})
	overflowCancelled := make(chan struct{})
	var overflowCancel sync.Once
	receiverC := newEndpointTestConnection("receiver-c", nil, func() {
		overflowCancel.Do(func() { close(overflowCancelled) })
	})
	if err := registry.BeginRegistration(fixture.init, sender.ref); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(
		fixture.init.ShareID, sender.ref, verifiedEndpointDescriptor(t, fixture),
	); err != nil {
		t.Fatal(err)
	}
	oldSession, err := registry.Join(fixture.init.ShareID, receiverA.ref)
	if err != nil || oldSession.Status != v2route.JoinReady {
		t.Fatalf("old join = %+v, %v", oldSession, err)
	}
	healthySession, err := registry.Join(fixture.init.ShareID, receiverB.ref)
	if err != nil || healthySession.Status != v2route.JoinReady {
		t.Fatalf("healthy join = %+v, %v", healthySession, err)
	}
	overflowSession, err := registry.Join(fixture.init.ShareID, receiverC.ref)
	if err != nil || overflowSession.Status != v2route.JoinReady {
		t.Fatalf("overflow join = %+v, %v", overflowSession, err)
	}

	sender.setRole(roleSender, fixture.init.ShareID)
	sender.addSession(oldSession.RelaySessionID)
	sender.addSession(healthySession.RelaySessionID)
	sender.addSession(overflowSession.RelaySessionID)
	receiverA.setRole(roleReceiver, fixture.init.ShareID)
	receiverA.addSession(oldSession.RelaySessionID)
	receiverB.setRole(roleReceiver, fixture.init.ShareID)
	receiverB.addSession(healthySession.RelaySessionID)
	receiverC.setRole(roleReceiver, fixture.init.ShareID)
	receiverC.addSession(overflowSession.RelaySessionID)
	server := endpointTestServer(t, registry, sender, receiverA, receiverB, receiverC)

	// Model the exact cleanup interval where the receiver is no longer a live
	// endpoint but Registry has not yet retired its active session.
	server.connections.detach(receiverA.ref)
	oldFrame, err := (v2.OpaqueRoute{
		RelaySessionID: oldSession.RelaySessionID, Ciphertext: []byte("old"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.forwardFrame(sender, oldFrame); err != nil {
		t.Fatalf("active route with vanished receiver closed sender: %v", err)
	}
	if err := server.forwardFrame(sender, oldFrame); err != nil {
		t.Fatalf("exact retired sender frame was not isolated: %v", err)
	}
	retired, err := registry.ResolveSession(oldSession.RelaySessionID, sender.ref)
	if err != nil || retired.Disposition != v2route.SessionRetired {
		t.Fatalf("old route = %+v, %v", retired, err)
	}

	healthyFrame, err := (v2.OpaqueRoute{
		RelaySessionID: healthySession.RelaySessionID, Ciphertext: []byte("healthy"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.forwardFrame(sender, healthyFrame); err != nil {
		t.Fatalf("healthy sibling route after retirement: %v", err)
	}
	forwarded, ok := receiverB.takeForward()
	if !ok || !bytes.Equal(forwarded, healthyFrame) {
		t.Fatalf("healthy sibling frame = %x, present=%t", forwarded, ok)
	}

	overflowFrame, err := (v2.OpaqueRoute{
		RelaySessionID: overflowSession.RelaySessionID, Ciphertext: []byte("overflow"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for range MaximumSessionQueueFrames {
		if !receiverC.enqueueForward(overflowSession.RelaySessionID, overflowFrame) {
			t.Fatal("could not fill the exact receiver session queue")
		}
	}
	if err := server.forwardFrame(sender, overflowFrame); err != nil {
		t.Fatalf("receiver queue overflow closed multiplexed sender: %v", err)
	}
	select {
	case <-overflowCancelled:
	default:
		t.Fatal("overflowing receiver session was not cancelled")
	}
	if resolution, err := registry.ResolveSession(overflowSession.RelaySessionID, sender.ref); err != nil ||
		resolution.Disposition != v2route.SessionRetired {
		t.Fatalf("overflowing session retirement = %+v, %v", resolution, err)
	}
	if err := server.forwardFrame(sender, healthyFrame); err != nil {
		t.Fatalf("healthy sibling route after overflow: %v", err)
	}
	if forwarded, ok := receiverB.takeForward(); !ok || !bytes.Equal(forwarded, healthyFrame) {
		t.Fatalf("healthy frame after overflow = %x, present=%t", forwarded, ok)
	}

	outsider := newEndpointTestConnection("replacement-sender", nil, func() {})
	outsider.setRole(roleSender, fixture.init.ShareID)
	if err := server.forwardFrame(outsider, oldFrame); !errors.Is(err, ErrProtocol) {
		t.Fatalf("retired outsider error = %v", err)
	}
	unknownID := oldSession.RelaySessionID
	unknownID[0] ^= 0xff
	unknownFrame, err := (v2.OpaqueRoute{
		RelaySessionID: unknownID, Ciphertext: []byte("forged"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.forwardFrame(sender, unknownFrame); !errors.Is(err, ErrProtocol) {
		t.Fatalf("never-authorized session error = %v", err)
	}
	clientRetirement, _ := (v2.SessionRetired{RelaySessionID: oldSession.RelaySessionID}).MarshalBinary()
	if err := server.forwardFrame(sender, clientRetirement); !errors.Is(err, ErrProtocol) {
		t.Fatalf("client-origin SESSION_RETIRED error = %v", err)
	}
}

func TestSessionRetiredControlQueueFailureClosesPeer(t *testing.T) {
	cancelled := make(chan struct{})
	var cancel sync.Once
	peer := newEndpointTestConnection("sender", nil, func() {
		cancel.Do(func() { close(cancelled) })
	})
	for range cap(peer.control) {
		peer.control <- controlWrite{data: []byte{1}, done: make(chan error, 1)}
	}
	server := &Server{}
	server.notifySessionRetired(peer, relaySessionIDForEndpointTest(91))
	select {
	case <-cancelled:
	default:
		t.Fatal("undeliverable SESSION_RETIRED did not fail-close the peer")
	}
}

func TestSenderCleanupRetiresReceiverBeforeEndpointSessionPublication(t *testing.T) {
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 2, MaxSessions: 8, MaxSessionsPerShare: 4,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	sender := newEndpointTestConnection("sender", nil, func() {})
	receiverCancelled := make(chan struct{})
	var cancel sync.Once
	receiver := newEndpointTestConnection("receiver", nil, func() {
		cancel.Do(func() { close(receiverCancelled) })
	})
	if err := registry.BeginRegistration(fixture.init, sender.ref); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender.ref, verifiedEndpointDescriptor(t, fixture)); err != nil {
		t.Fatal(err)
	}
	joined, err := registry.Join(fixture.init.ShareID, receiver.ref)
	if err != nil || joined.Status != v2route.JoinReady {
		t.Fatalf("join = %+v, %v", joined, err)
	}

	sender.setRole(roleSender, fixture.init.ShareID)
	// The Registry session exists, but descriptor delivery has not yet published
	// the session into either endpoint index. Exact Registry participants must
	// still make sender cleanup cancel this receiver.
	server := endpointTestServer(t, registry, sender, receiver)
	server.cleanup(sender)
	select {
	case <-receiverCancelled:
	default:
		t.Fatal("sender cleanup orphaned a receiver joined before endpoint publication")
	}
	if resolution, resolveErr := registry.ResolveSession(joined.RelaySessionID, sender.ref); resolveErr != nil || resolution.Disposition != v2route.SessionRetired {
		t.Fatalf("cleanup route = %+v, %v", resolution, resolveErr)
	}
	if current, _, _ := server.connections.resolve(sender.ref); current != nil {
		t.Fatal("sender remained endpoint-visible after authoritative Registry cleanup")
	}
}

func TestReceiverSessionActivationClosesJoinStopRaceWithoutStaleMembership(t *testing.T) {
	newFixture := func(t *testing.T) (*v2route.Registry, endpointFixture, *Server, *connection, *connection) {
		t.Helper()
		registry, err := v2route.New(context.Background(), v2route.Config{
			MaxRoutes: 2, MaxSessions: 8, MaxSessionsPerShare: 4,
			Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture := newEndpointFixture(t)
		sender := newEndpointTestConnection("sender", nil, func() {})
		receiver := newEndpointTestConnection("receiver", nil, func() {})
		if err := registry.BeginRegistration(fixture.init, sender.ref); err != nil {
			t.Fatal(err)
		}
		if err := registry.Publish(fixture.init.ShareID, sender.ref, verifiedEndpointDescriptor(t, fixture)); err != nil {
			t.Fatal(err)
		}
		sender.setRole(roleSender, fixture.init.ShareID)
		server := endpointTestServer(t, registry, sender, receiver)
		return registry, fixture, server, sender, receiver
	}

	t.Run("STOP between Registry Join and endpoint install", func(t *testing.T) {
		registry, fixture, server, sender, receiver := newFixture(t)
		joined, err := registry.Join(fixture.init.ShareID, receiver.ref)
		if err != nil || joined.Status != v2route.JoinReady {
			t.Fatalf("join = %+v, %v", joined, err)
		}
		stop, authority := endpointStop(t, fixture)
		retirement, err := registry.Stop(context.Background(), stop, authority)
		if err != nil {
			t.Fatal(err)
		}
		server.applyRouteRetirement(retirement, RetirementSourceStop)
		if server.activateReceiverSession(receiver, fixture.init.ShareID, joined) {
			t.Fatal("retired Registry session was republished for descriptor delivery")
		}
		if len(sender.sessionIDs()) != 0 || len(receiver.sessionIDs()) != 0 ||
			len(sender.forward) != 0 || len(receiver.forward) != 0 {
			t.Fatalf("post-retirement install leaked state: sender=%v receiver=%v", sender.sessionIDs(), receiver.sessionIDs())
		}
	})

	t.Run("STOP after install removes queues and leaves unrelated endpoint state", func(t *testing.T) {
		registry, fixture, server, sender, receiver := newFixture(t)
		joined, err := registry.Join(fixture.init.ShareID, receiver.ref)
		if err != nil || !server.activateReceiverSession(receiver, fixture.init.ShareID, joined) {
			t.Fatalf("session activation = %+v, %v", joined, err)
		}
		frame, _ := (v2.OpaqueRoute{
			RelaySessionID: joined.RelaySessionID, Ciphertext: []byte("queued-before-stop"),
		}).MarshalBinary()
		if !receiver.enqueueForward(joined.RelaySessionID, frame) {
			t.Fatal("active receiver queue rejected frame")
		}
		healthy := newEndpointTestConnection("unrelated", nil, func() {})
		healthySession := relaySessionIDForEndpointTest(93)
		healthy.addSession(healthySession)
		if !healthy.enqueueForward(healthySession, []byte("healthy")) {
			t.Fatal("unrelated endpoint rejected frame")
		}
		if !server.connections.add(healthy) {
			t.Fatal("could not add unrelated exact connection")
		}
		t.Cleanup(func() { server.connections.complete(healthy.ref) })

		stop, authority := endpointStop(t, fixture)
		retirement, err := registry.Stop(context.Background(), stop, authority)
		if err != nil {
			t.Fatal(err)
		}
		server.applyRouteRetirement(retirement, RetirementSourceStop)
		if len(sender.sessionIDs()) != 0 || len(receiver.sessionIDs()) != 0 || len(receiver.forward) != 0 ||
			len(receiver.forwardOrder) != 0 || receiver.forwardFrames != 0 || receiver.forwardBytes != 0 {
			t.Fatalf("committed STOP leaked retired endpoint state: sender=%v receiver=%v",
				sender.sessionIDs(), receiver.sessionIDs())
		}
		if len(healthy.sessionIDs()) != 1 || len(healthy.forward) != 1 || healthy.forwardFrames != 1 {
			t.Fatalf("exact retirement damaged unrelated endpoint state: sessions=%v queues=%d frames=%d",
				healthy.sessionIDs(), len(healthy.forward), healthy.forwardFrames)
		}
	})
}

func TestConcurrentResumeLoserCleanupCannotEraseWinnerOrStopAuthority(t *testing.T) {
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 2, MaxSessions: 8, MaxSessionsPerShare: 4,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	oldSender := endpointTestConnectionRef("sender-old")
	if err := registry.BeginRegistration(fixture.init, oldSender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, oldSender, verifiedEndpointDescriptor(t, fixture)); err != nil {
		t.Fatal(err)
	}
	if _, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, oldSender); !transitioned {
		t.Fatal("old sender did not enter crash grace")
	}
	resume := fixture.init
	resume.Mode = v2.RegistrationResume
	if err := registry.ValidateResumeCredential(resume, fixture.token); err != nil {
		t.Fatalf("candidate A precheck: %v", err)
	}
	if err := registry.ValidateResumeCredential(resume, fixture.token); err != nil {
		t.Fatalf("candidate B precheck: %v", err)
	}
	authority := endpointResumeAuthority(t, fixture, resume)
	type resumeResult struct {
		connection v2route.ConnectionRef
		err        error
	}
	start := make(chan struct{})
	results := make(chan resumeResult, 2)
	for _, candidate := range []v2route.ConnectionRef{
		endpointTestConnectionRef("candidate-a"), endpointTestConnectionRef("candidate-b"),
	} {
		go func(candidate v2route.ConnectionRef) {
			<-start
			results <- resumeResult{connection: candidate, err: registry.Resume(resume, authority, candidate, fixture.token)}
		}(candidate)
	}
	close(start)
	first, second := <-results, <-results
	var winner, loser v2route.ConnectionRef
	switch {
	case first.err == nil && errors.Is(second.err, v2route.ErrNotFound):
		winner, loser = first.connection, second.connection
	case second.err == nil && errors.Is(first.err, v2route.ErrNotFound):
		winner, loser = second.connection, first.connection
	default:
		t.Fatalf("concurrent Resume results = %+v, %+v", first, second)
	}

	winnerCancelled := make(chan struct{})
	var cancelWinner sync.Once
	winnerPeer := newConnection(winner, nil, func() { cancelWinner.Do(func() { close(winnerCancelled) }) })
	winnerPeer.setRole(roleSender, fixture.init.ShareID)
	loserPeer := newConnection(loser, nil, func() {})
	receiverCancelled := make(chan struct{})
	var cancelReceiver sync.Once
	receiver := newEndpointTestConnection("receiver", nil, func() { cancelReceiver.Do(func() { close(receiverCancelled) }) })
	server := endpointTestServer(t, registry, winnerPeer, loserPeer, receiver)
	joined, err := registry.Join(fixture.init.ShareID, receiver.ref)
	if err != nil || joined.Sender != winner || !server.activateReceiverSession(receiver, fixture.init.ShareID, joined) {
		t.Fatalf("winner join/activation = %+v, %v", joined, err)
	}

	server.cleanup(loserPeer)
	if resolution, resolveErr := registry.ResolveSession(joined.RelaySessionID, winner); resolveErr != nil || resolution.Disposition != v2route.SessionForward || resolution.Destination != receiver.ref {
		t.Fatalf("loser cleanup erased winner route = %+v, %v", resolution, resolveErr)
	}
	stop, stopAuthority := endpointStop(t, fixture)
	retirement, err := registry.Stop(context.Background(), stop, stopAuthority)
	if err != nil || retirement.Owner != winner || len(retirement.Sessions) != 1 ||
		retirement.Sessions[0].Receiver != receiver.ref {
		t.Fatalf("STOP retirement after concurrent Resume = %+v, %v", retirement, err)
	}
	server.applyRouteRetirement(retirement, RetirementSourceStop)
	select {
	case <-winnerCancelled:
	default:
		t.Fatal("STOP did not cancel exact Resume winner")
	}
	select {
	case <-receiverCancelled:
	default:
		t.Fatal("STOP did not cancel winner receiver")
	}
}

func TestForwardQueueCannotReappearAfterSessionRemoval(t *testing.T) {
	peer := newEndpointTestConnection("receiver", nil, func() {})
	sessionID := relaySessionIDForEndpointTest(92)
	peer.addSession(sessionID)
	if !peer.enqueueForward(sessionID, []byte("queued")) {
		t.Fatal("active session rejected initial forward")
	}
	if !peer.removeSession(sessionID) {
		t.Fatal("active session was not removed")
	}
	if peer.enqueueForward(sessionID, []byte("late")) {
		t.Fatal("forward queue was recreated after session removal")
	}
	if frame, ok := peer.takeForward(); ok || frame != nil || len(peer.forward) != 0 ||
		len(peer.forwardOrder) != 0 || peer.forwardFrames != 0 || peer.forwardBytes != 0 {
		t.Fatalf("retired forward state = frame %q ok=%t queues=%d order=%d frames=%d bytes=%d",
			frame, ok, len(peer.forward), len(peer.forwardOrder), peer.forwardFrames, peer.forwardBytes)
	}
}

func TestCommittedStopCleansExactPeersBeforeStoppedWriteFailure(t *testing.T) {
	const relayBase = "https://relay.example/stop-write-failure"
	endpoint, err := v2.NormalizeRelayEndpoint(relayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 4, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 16, Random: &sequenceReader{next: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	var connectionSequence atomic.Uint64
	server, err := New(Config{
		Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity,
		ConnectionIDs: ConnectionIDSourceFunc(func() (v2route.ConnectionID, error) {
			return v2route.ConnectionID(fmt.Sprintf("stop-write-%d", connectionSequence.Add(1))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	dial := memoryServerDialer(server)
	sender, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
		RelayBaseURL: relayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver := dialReceiver(t, relayBase, fixture.init.ShareID, dial)
	defer receiver.Close()

	stopServeDone := make(chan error, 1)
	stopDial := func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		failing := &failNthWriteSocket{
			BinaryConnection: relay,
			failAt:           2, // Challenge succeeds; committed STOPPED response fails.
			err:              errors.New("injected STOPPED write failure"),
		}
		go func() { stopServeDone <- server.Serve(context.Background(), failing) }()
		return client, nil
	}
	if err := relayv2.Stop(context.Background(), relayv2.StopConfig{
		RelayBaseURL: relayBase, ShareID: fixture.init.ShareID, ShareInstance: fixture.init.ShareInstance,
		PKHash: fixture.init.PKHash, StopID: v2.StopID{1}, SenderPrivateKey: fixture.privateKey,
		Dial: relayv2.DialOptions{SocketDialer: stopDial},
	}); err == nil {
		t.Fatal("STOP unexpectedly acknowledged after injected WS2Y failure")
	}
	select {
	case <-sender.Done():
	case <-time.After(time.Second):
		t.Fatal("committed STOP did not close sender before WS2Y failure returned")
	}
	select {
	case <-receiver.Done():
	case <-time.After(time.Second):
		t.Fatal("committed STOP did not close receiver before WS2Y failure returned")
	}
	select {
	case err := <-stopServeDone:
		if err == nil || !strings.Contains(err.Error(), "STOPPED write failure") {
			t.Fatalf("STOP server result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("STOP server did not settle after WS2Y failure")
	}
	if result, joinErr := registry.Join(fixture.init.ShareID, endpointTestConnectionRef("late-receiver")); joinErr != nil || result.Status != v2route.JoinStopped {
		t.Fatalf("committed STOP was rolled back after WS2Y failure: %+v, %v", result, joinErr)
	}
}

func relaySessionIDForEndpointTest(value byte) v2.RelaySessionID {
	var sessionID v2.RelaySessionID
	sessionID[0] = value
	return sessionID
}

func endpointTestConnectionRef(id v2route.ConnectionID) v2route.ConnectionRef {
	reference, err := v2route.NewConnectionRef(id)
	if err != nil {
		panic(err)
	}
	return reference
}

func newEndpointTestConnection(
	id v2route.ConnectionID,
	socket BinaryConnection,
	cancel context.CancelFunc,
) *connection {
	return newConnection(endpointTestConnectionRef(id), socket, cancel)
}

func endpointTestServer(
	t *testing.T,
	registry *v2route.Registry,
	peers ...*connection,
) *Server {
	t.Helper()
	connections := newConnectionRegistry()
	for _, peer := range peers {
		if !connections.add(peer) {
			t.Fatalf("add endpoint test connection %q", peer.ref.ConnectionID())
		}
	}
	t.Cleanup(func() {
		for _, peer := range peers {
			connections.complete(peer.ref)
		}
	})
	return &Server{registry: registry, connections: connections}
}

func endpointCurrentConnectionRef(
	t *testing.T,
	server *Server,
	id v2route.ConnectionID,
) v2route.ConnectionRef {
	t.Helper()
	server.connections.mu.Lock()
	defer server.connections.mu.Unlock()
	peer := server.connections.current[id]
	if peer == nil {
		t.Fatalf("connection %q is not current", id)
	}
	return peer.ref
}

func dialReceiver(
	t *testing.T,
	relayBase string,
	shareID v2.ShareID,
	dial func(context.Context, string, http.Header) (relayv2.BinarySocket, error),
) *relayv2.ReceiverConnection {
	t.Helper()
	receiver, err := relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: relayBase, ShareID: shareID, Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		t.Fatalf("join receiver: %v", err)
	}
	return receiver
}

func establishSession(
	t *testing.T,
	sender *relayv2.SenderConnection,
	receiver *relayv2.ReceiverConnection,
	first []byte,
) *relayv2.Channel {
	t.Helper()
	sent := make(chan error, 1)
	go func() { sent <- receiver.Channel().Send(context.Background(), first) }()
	channel, err := sender.Accept(context.Background())
	if err != nil {
		t.Fatalf("accept session: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("send first frame: %v", err)
	}
	return channel
}

func assertFrame(t *testing.T, frames <-chan framechannel.Frame, want string) {
	t.Helper()
	select {
	case frame := <-frames:
		if string(frame) != want {
			t.Fatalf("frame = %q, want %q", frame, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

type endpointFixture struct {
	init       v2.RegisterInit
	token      v2.ResumeToken
	descriptor []byte
	privateKey ed25519.PrivateKey
}

func newEndpointFixture(t *testing.T) endpointFixture {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x27}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	var fixture endpointFixture
	fixture.privateKey = privateKey
	pkDigest := sha256.Sum256(append([]byte("windshare/v2 sender-key\x00"), publicKey...))
	copy(fixture.init.PKHash[:], pkDigest[:v2.PKHashBytes])
	shareDigest := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), fixture.init.PKHash[:]...))
	copy(fixture.init.ShareID[:], shareDigest[:v2.ShareIDBytes])
	for index := range fixture.init.ShareInstance {
		fixture.init.ShareInstance[index] = byte(0x40 + index)
	}
	for index := range fixture.token {
		fixture.token[index] = byte(0x60 + index)
	}
	fixture.init.Mode = v2.RegistrationFresh
	fixture.init.ResumeTokenHash = sha256.Sum256(fixture.token[:])
	info := append([]byte("windshare/v2 descriptor\x00"), fixture.init.PKHash[:]...)
	key, err := hkdf.Key(sha256.New, bytes.Repeat([]byte{0x11}, 16), nil, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	fixture.descriptor, err = sealDescriptor(fixture.init, key, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	fixture.init.DescriptorDigest = sha256.Sum256(fixture.descriptor)
	return fixture
}

func verifiedEndpointDescriptor(t *testing.T, fixture endpointFixture) v2.VerifiedDescriptor {
	t.Helper()
	endpoint, err := v2.NormalizeRelayEndpoint("https://relay.example/unit-authority")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 1, Random: &sequenceReader{next: 91}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := v2.RegistrationChallengeBinding(fixture.init, endpoint.Identity)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := ledger.Issue(v2.ChallengeRegister, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := v2.NewRegisterProof(fixture.init, challenge, endpoint.Identity, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ledger.AuthenticateRegistration(
		challenge.ID, fixture.init, endpoint.Identity, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := v2.VerifyDescriptorUpload(
		fixture.init, authority, v2.DescriptorUpload{Object: fixture.descriptor},
	)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func endpointStop(t *testing.T, fixture endpointFixture) (v2.StopInit, v2.StopAuthority) {
	t.Helper()
	endpoint, err := v2.NormalizeRelayEndpoint("https://relay.example/unit-stop")
	if err != nil {
		t.Fatal(err)
	}
	stop := v2.StopInit{
		ShareID: fixture.init.ShareID, ShareInstance: fixture.init.ShareInstance,
		PKHash: fixture.init.PKHash, RelayIdentity: endpoint.Identity, StopID: v2.StopID{1},
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 1, Random: &sequenceReader{next: 121},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := v2.StopChallengeBinding(stop)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := ledger.Issue(v2.ChallengeStop, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := v2.NewStopProof(stop, challenge, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ledger.AuthenticateStop(challenge.ID, stop, proof)
	if err != nil {
		t.Fatal(err)
	}
	return stop, authority
}

func endpointResumeAuthority(
	t *testing.T,
	fixture endpointFixture,
	resume v2.RegisterInit,
) v2.SenderAuthority {
	t.Helper()
	endpoint, err := v2.NormalizeRelayEndpoint("https://relay.example/unit-resume")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 1, Random: &sequenceReader{next: 151},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := v2.RegistrationChallengeBinding(resume, endpoint.Identity)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := ledger.Issue(v2.ChallengeResume, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := v2.NewRegisterProof(resume, challenge, endpoint.Identity, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ledger.AuthenticateRegistration(challenge.ID, resume, endpoint.Identity, proof)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func sealDescriptor(init v2.RegisterInit, key []byte, privateKey ed25519.PrivateKey) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext := []byte{0xa1, 0x00, 0x01}
	header := make([]byte, 8)
	header[0] = v2.WireVersion
	binary.BigEndian.PutUint32(header[4:], uint32(len(plaintext)+aead.Overhead()))
	objectContext := append([]byte{v2.Suite}, init.PKHash[:]...)
	objectContext = append(objectContext, init.ShareID[:]...)
	contextHash := sha256.Sum256(objectContext)
	aad := append([]byte("windshare/v2 object/descriptor\x00"), contextHash[:]...)
	aad = append(aad, header...)
	nonce := bytes.Repeat([]byte{0x18}, aead.NonceSize())
	prefix := append(bytes.Clone(header), nonce...)
	prefix = aead.Seal(prefix, nonce, plaintext, aad)
	preimage := append([]byte("windshare/v2 object/descriptor\x00"), contextHash[:]...)
	preimage = append(preimage, prefix...)
	return append(prefix, ed25519.Sign(privateKey, preimage)...), nil
}
