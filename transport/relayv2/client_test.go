package relayv2

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	framechannel "github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestDialClientsAndOpaqueChannels(t *testing.T) {
	fixture := newClientFixture(t)
	t.Run("fresh sender", func(t *testing.T) {
		socket := newScriptedSocket()
		traces := make(chan LifecycleTrace, 16)
		socket.respond(challengeFrame(t, v2.ChallengeRegister))
		registered, _ := (v2.Registered{
			ShareID: fixture.fresh.ShareID, ShareInstance: fixture.fresh.ShareInstance,
			DescriptorDigest: fixture.fresh.DescriptorDigest,
		}).MarshalBinary()
		socket.respond(registered)
		sender, err := DialSender(context.Background(), SenderConfig{
			RelayBaseURL: "https://relay.example/base?token=x", Init: fixture.fresh,
			SenderPrivateKey: fixture.privateKey, Descriptor: fixture.descriptor,
			Dial: DialOptions{
				Header: http.Header{"Origin": {"https://app.example"}}, SocketDialer: socket.dial,
				LifecycleTracer: LifecycleTraceFunc(func(event LifecycleTrace) { traces <- event }),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if sender.Endpoint().DialURL != "wss://relay.example/base/v2/ws?token=x" {
			t.Fatalf("dial URL = %q", sender.Endpoint().DialURL)
		}
		if stats := sender.RegistrationStats(); stats.BytesSent == 0 || stats.BytesReceived == 0 {
			t.Fatalf("registration stats = %+v", stats)
		}
		assertWriteMagic(t, socket, "WS2R")
		assertWriteMagic(t, socket, "WS2P")
		assertWriteMagic(t, socket, "WS2U")

		sessionID := relaySessionID(1)
		route, _ := (v2.OpaqueRoute{RelaySessionID: sessionID, Ciphertext: []byte("incoming")}).MarshalBinary()
		socket.respond(route)
		channel, err := sender.Accept(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if channel.RelaySessionID() != sessionID || string(<-channel.Recv()) != "incoming" {
			t.Fatal("sender channel did not preserve route identity and ciphertext")
		}
		if err := channel.SendTerminal(context.Background(), framechannel.Frame("outgoing")); err != nil {
			t.Fatal(err)
		}
		assertOpaqueWrite(t, socket, sessionID, "outgoing")
		admitted := waitLifecycleTrace(t, traces, LifecycleSendAdmitted)
		if admitted.LinkID == 0 || admitted.OperationID == 0 || admitted.RelaySessionID != sessionID ||
			!admitted.Terminal || admitted.Disposition != framechannel.SendAccepted {
			t.Fatalf("public lifecycle trace = %+v", admitted)
		}
		if channel.State() != framechannel.Closed || channel.Err() != nil {
			t.Fatalf("terminal state/error = %v/%v", channel.State(), channel.Err())
		}
		_ = sender.Close()
		<-sender.Done()
	})

	t.Run("resume sender", func(t *testing.T) {
		socket := newScriptedSocket()
		socket.respond(challengeFrame(t, v2.ChallengeResume))
		registered, _ := (v2.Registered{
			ShareID: fixture.fresh.ShareID, ShareInstance: fixture.fresh.ShareInstance,
			DescriptorDigest: fixture.fresh.DescriptorDigest,
		}).MarshalBinary()
		socket.respond(registered)
		resume, _ := ResumeInit(fixture.fresh)
		sender, err := DialSender(context.Background(), SenderConfig{
			RelayBaseURL: "wss://relay.example", Init: resume, ResumeToken: fixture.token,
			SenderPrivateKey: fixture.privateKey, Dial: DialOptions{SocketDialer: socket.dial},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertWriteMagic(t, socket, "WS2R")
		assertWriteMagic(t, socket, "WS2T")
		assertWriteMagic(t, socket, "WS2P")
		_ = sender.Close()
	})

	t.Run("receiver", func(t *testing.T) {
		socket := newScriptedSocket()
		sessionID := relaySessionID(3)
		delivery, _ := (v2.DescriptorDelivery{RelaySessionID: sessionID, Object: fixture.descriptor}).MarshalBinary()
		socket.respond(delivery)
		receiver, err := DialReceiver(context.Background(), ReceiverConfig{
			RelayBaseURL: "https://relay.example", ShareID: fixture.fresh.ShareID,
			Dial: DialOptions{SocketDialer: socket.dial},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(receiver.Descriptor(), fixture.descriptor) || receiver.Channel().RelaySessionID() != sessionID {
			t.Fatal("descriptor delivery changed")
		}
		assertWriteMagic(t, socket, "WS2J")
		if err := receiver.Channel().Send(context.Background(), []byte("request")); err != nil {
			t.Fatal(err)
		}
		assertOpaqueWrite(t, socket, sessionID, "request")
		route, _ := (v2.OpaqueRoute{RelaySessionID: sessionID, Ciphertext: []byte("response")}).MarshalBinary()
		socket.respond(route)
		if frame := <-receiver.Channel().Recv(); string(frame) != "response" {
			t.Fatalf("response = %q", frame)
		}
		_ = receiver.Close()
		<-receiver.Done()
	})

	t.Run("stop", func(t *testing.T) {
		socket := newScriptedSocket()
		socket.respond(challengeFrame(t, v2.ChallengeStop))
		var stopID v2.StopID
		stopID[0] = 9
		stopped, _ := (v2.Stopped{StopID: stopID}).MarshalBinary()
		socket.respond(stopped)
		err := Stop(context.Background(), StopConfig{
			RelayBaseURL: "https://relay.example", ShareID: fixture.fresh.ShareID,
			ShareInstance: fixture.fresh.ShareInstance, PKHash: fixture.fresh.PKHash,
			StopID: stopID, SenderPrivateKey: fixture.privateKey,
			Dial: DialOptions{SocketDialer: socket.dial},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertWriteMagic(t, socket, "WS2X")
		assertWriteMagic(t, socket, "WS2V")
	})
}

func TestClientErrorsAndQueueBounds(t *testing.T) {
	fixture := newClientFixture(t)
	if _, err := NewFreshRegisterInit(v2.ShareID{}, v2.ShareInstance{}, v2.PKHash{}, nil, v2.ResumeToken{}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty fresh init error = %v", err)
	}
	if _, err := ResumeInit(v2.RegisterInit{}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty resume error = %v", err)
	}
	if _, err := DialSender(context.Background(), SenderConfig{RelayBaseURL: "bad"}); err == nil {
		t.Fatal("invalid relay URL accepted")
	}
	if _, err := DialSender(context.Background(), SenderConfig{
		RelayBaseURL: "https://relay.example", Init: fixture.fresh,
	}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("missing sender key error = %v", err)
	}

	socket := newScriptedSocket()
	errorFrame, _ := (v2.ErrorFrame{Code: v2.ErrorNotFound}).MarshalBinary()
	socket.respond(errorFrame)
	_, err := DialReceiver(context.Background(), ReceiverConfig{
		RelayBaseURL: "https://relay.example", ShareID: fixture.fresh.ShareID,
		Dial: DialOptions{SocketDialer: socket.dial},
	})
	var relayError *RelayError
	if !errors.As(err, &relayError) || relayError.Code != v2.ErrorNotFound || relayError.Error() == "" {
		t.Fatalf("relay error = %v", err)
	}

	linkContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := newScriptedSocket()
	l := newLink(linkContext, blocked, false)
	id := relaySessionID(5)
	boundedChannel := newChannel(id, l)
	l.channels[id] = boundedChannel
	queue := &sendQueue{requests: make([]*sendRequest, channelSendFrames)}
	for index := range queue.requests {
		queue.requests[index] = &sendRequest{receipt: make(chan error, 1)}
	}
	l.queues[id] = queue
	l.order = []v2.RelaySessionID{id}
	l.queued = channelSendFrames
	if err := l.enqueue(context.Background(), boundedChannel, &sendRequest{receipt: make(chan error, 1)}); !errors.Is(err, ErrEgressOverflow) ||
		framechannel.SendDispositionOf(err) != framechannel.SendRejected {
		t.Fatalf("egress bound error = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
	if err := boundedChannel.Close(); err != nil {
		t.Fatal(err)
	}

	channel := newChannel(relaySessionID(6), l)
	for range channelReceiveFrames {
		if !channel.deliver([]byte{1}) {
			t.Fatal("receive queue filled early")
		}
	}
	if channel.deliver([]byte{2}) {
		t.Fatal("receive queue exceeded bound")
	}
	l.failChannel(channel, LifecycleRetirementIngressFailure, ErrIngressOverflow)
	if !errors.Is(channel.Err(), ErrIngressOverflow) {
		t.Fatalf("channel close reason = %v", channel.Err())
	}

	fixed := newLink(context.Background(), newScriptedSocket(), true)
	if created, ok := fixed.channel(relaySessionID(7)); created != nil || ok {
		t.Fatal("fixed receiver accepted an unknown session")
	}
	fixed.stop(nil)
	if _, ok := fixed.channel(relaySessionID(8)); ok {
		t.Fatal("closed link created a channel")
	}
}

func TestSessionRetiredControlClosesOnlyExactChannelAndIsIdempotent(t *testing.T) {
	socket := newScriptedSocket()
	l := newLink(context.Background(), socket, false)
	old := l.installFixed(relaySessionID(21))
	sibling := l.installFixed(relaySessionID(22))
	l.start()
	defer l.stop(nil)

	retired, _ := (v2.SessionRetired{RelaySessionID: old.id}).MarshalBinary()
	socket.respond(retired)
	select {
	case _, open := <-old.Recv():
		if open || old.State() != framechannel.Closed || !errors.Is(old.Err(), ErrSessionRetired) {
			t.Fatalf("retired channel open=%t state=%d error=%v", open, old.State(), old.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("retirement control did not close the exact channel")
	}
	if err := old.Send(context.Background(), framechannel.Frame("after-retirement")); !errors.Is(err, ErrSessionRetired) || framechannel.SendDispositionOf(err) != framechannel.SendRetired {
		t.Fatalf("send after relay retirement = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}

	// Duplicate, never-materialized, and locally closed IDs are allocation-free
	// terminal no-ops; none may disturb a healthy sibling.
	socket.respond(retired)
	unknownID := relaySessionID(23)
	unknown, _ := (v2.SessionRetired{RelaySessionID: unknownID}).MarshalBinary()
	socket.respond(unknown)
	locallyClosed := l.installFixed(relaySessionID(24))
	if err := locallyClosed.Close(); err != nil {
		t.Fatal(err)
	}
	closedControl, _ := (v2.SessionRetired{RelaySessionID: locallyClosed.id}).MarshalBinary()
	socket.respond(closedControl)
	siblingRoute, _ := (v2.OpaqueRoute{
		RelaySessionID: sibling.id, Ciphertext: []byte("healthy-after-retirement"),
	}).MarshalBinary()
	socket.respond(siblingRoute)
	select {
	case frame := <-sibling.Recv():
		if string(frame) != "healthy-after-retirement" {
			t.Fatalf("sibling frame = %q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("retirement controls stalled the healthy sibling")
	}
	l.channelMu.Lock()
	_, unknownAllocated := l.channels[unknownID]
	l.channelMu.Unlock()
	if unknownAllocated {
		t.Fatal("unknown retirement control allocated channel state")
	}
	select {
	case <-l.done:
		t.Fatalf("idempotent retirement ended link: %v", l.Err())
	default:
	}

	malformed := append(bytes.Clone(retired), 0)
	socket.respond(malformed)
	select {
	case <-l.done:
		if !errors.Is(l.Err(), ErrProtocol) {
			t.Fatalf("malformed retirement link error = %v", l.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("malformed retirement did not fail-close the link")
	}
}

func TestChannelCloseAndCancellationCannotLeaveOrRecreateQueuedWrites(t *testing.T) {
	l := newLink(context.Background(), newScriptedSocket(), false)
	channel := l.installFixed(relaySessionID(9))

	ctx, cancel := context.WithCancel(context.Background())
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- channel.Send(ctx, framechannel.Frame("queued"))
	}()
	requireQueuedRequests(t, l, channel.id, 1)
	cancel()
	if err := <-sendDone; !errors.Is(err, context.Canceled) ||
		framechannel.SendDispositionOf(err) != framechannel.SendRejected {
		t.Fatalf("canceled send error = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
	requireQueuedRequests(t, l, channel.id, 0)

	// Holding channelMu places Send exactly between its state check and
	// registration check. Close must drain the insertion and leave no queue that
	// a late sender can resurrect after the channel disappears from the link.
	l.channelMu.Lock()
	lateSendDone := make(chan error, 1)
	go func() {
		lateSendDone <- channel.Send(context.Background(), framechannel.Frame("late"))
	}()
	requireMutexHeld(t, &channel.mu)
	closeDone := make(chan error, 1)
	go func() { closeDone <- channel.Close() }()
	l.channelMu.Unlock()
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-lateSendDone; !errors.Is(err, ErrClosed) ||
		framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
		t.Fatalf("send racing close error = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
	requireQueuedRequests(t, l, channel.id, 0)
	if err := channel.Send(context.Background(), framechannel.Frame("after-close")); !errors.Is(err, ErrClosed) ||
		framechannel.SendDispositionOf(err) != framechannel.SendRetired {
		t.Fatalf("send after natural close = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}

	acceptedLink := newLink(context.Background(), newScriptedSocket(), false)
	acceptedChannel := acceptedLink.installFixed(relaySessionID(10))
	acceptedDone := make(chan error, 1)
	go func() {
		acceptedDone <- acceptedChannel.Send(context.Background(), framechannel.Frame("accepted"))
	}()
	requireQueuedRequests(t, acceptedLink, acceptedChannel.id, 1)
	acceptedErr := errors.New("link failed after queue admission")
	acceptedLink.stop(acceptedErr)
	if err := <-acceptedDone; !errors.Is(err, acceptedErr) ||
		framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
		t.Fatalf("post-admission link failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
	failedLink := newLink(context.Background(), newScriptedSocket(), false)
	failedChannel := failedLink.installFixed(relaySessionID(11))
	linkErr := errors.New("link failed before admission")
	failedLink.stop(linkErr)
	if err := failedChannel.Send(context.Background(), framechannel.Frame("after-failure")); !errors.Is(err, linkErr) || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
		t.Fatalf("send after link failure = %v disposition=%d", err, framechannel.SendDispositionOf(err))
	}
}

func TestRelayTerminalReservationPreservesRejectedChannelAndCannotBeOvertakenByClose(t *testing.T) {
	t.Run("pre-admission rejection keeps channel open", func(t *testing.T) {
		link := newLink(context.Background(), newScriptedSocket(), false)
		channel := link.installFixed(relaySessionID(12))
		if err := channel.SendTerminal(context.Background(), nil); !errors.Is(err, ErrFrameBounds) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("invalid terminal = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if channel.State() != framechannel.Open {
			t.Fatalf("invalid terminal changed state=%d", channel.State())
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := channel.SendTerminal(ctx, framechannel.Frame("terminal")); !errors.Is(err, context.Canceled) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("canceled terminal = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if channel.State() != framechannel.Open {
			t.Fatalf("canceled terminal changed state=%d", channel.State())
		}

		link.writeMu.Lock()
		link.queued = connectionSendFrames
		link.writeMu.Unlock()
		if err := channel.SendTerminal(context.Background(), framechannel.Frame("terminal")); !errors.Is(err, ErrEgressOverflow) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("overflow terminal = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		link.writeMu.Lock()
		link.queued = 0
		link.writeMu.Unlock()
		if channel.State() != framechannel.Open {
			t.Fatalf("overflow terminal changed state=%d", channel.State())
		}
	})

	t.Run("admitted terminal owns close ordering", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			link := newLink(context.Background(), newScriptedSocket(), false)
			channel := link.installFixed(relaySessionID(13))
			terminalResult := make(chan error, 1)
			go func() {
				terminalResult <- channel.SendTerminal(context.Background(), framechannel.Frame("terminal"))
			}()
			synctest.Wait()
			request, ok := link.takeRequest()
			if !ok {
				t.Fatal("terminal did not reach relay queue admission")
			}
			closeResult := make(chan error, 1)
			go func() { closeResult <- channel.Close() }()
			synctest.Wait()
			select {
			case err := <-closeResult:
				t.Fatalf("Close overtook terminal settlement: %v", err)
			default:
			}
			request.receipt <- nil
			synctest.Wait()
			if err := <-terminalResult; err != nil {
				t.Fatalf("terminal settlement: %v", err)
			}
			if err := <-closeResult; err != nil {
				t.Fatalf("Close after terminal settlement: %v", err)
			}
			if channel.State() != framechannel.Closed {
				t.Fatalf("terminal state=%d", channel.State())
			}
		})
	})
}

func TestRelayCloseAndTerminalReservationHaveOneLinearization(t *testing.T) {
	t.Run("published reservation wins before queue insertion", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleTerminalReserved)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(31))

		terminalResult := make(chan error, 1)
		go func() {
			terminalResult <- channel.SendTerminal(context.Background(), framechannel.Frame("terminal"))
		}()
		gate.wait(t)

		closeResult := make(chan error, 1)
		go func() { closeResult <- channel.Close() }()
		deferred := gate.waitFor(t, LifecycleRetirementDeferred)
		if deferred.RetirementSource != LifecycleRetirementLocalClose || !deferred.Terminal {
			t.Fatalf("deferred close trace = %+v", deferred)
		}
		select {
		case err := <-closeResult:
			t.Fatalf("Close overtook the published reservation: %v", err)
		default:
		}

		gate.release()
		requireQueuedRequests(t, link, channel.id, 1)
		request, ok := link.takeRequest()
		if !ok {
			t.Fatal("reserved terminal was not admitted")
		}
		select {
		case err := <-closeResult:
			t.Fatalf("Close overtook admitted terminal settlement: %v", err)
		default:
		}
		request.receipt <- nil
		if err := <-terminalResult; err != nil {
			t.Fatalf("terminal settlement: %v", err)
		}
		if err := <-closeResult; err != nil {
			t.Fatalf("Close after terminal settlement: %v", err)
		}
		if channel.State() != framechannel.Closed || channel.Err() != nil {
			t.Fatalf("terminal/close state=%d error=%v", channel.State(), channel.Err())
		}
	})

	t.Run("close winner rejects a later terminal as retired", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleRetired)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(32))

		closeResult := make(chan error, 1)
		go func() { closeResult <- channel.Close() }()
		event := gate.wait(t)
		if event.OperationID == 0 || event.RetirementSource != LifecycleRetirementLocalClose ||
			event.Cause != LifecycleCauseNone || event.DrainCause != LifecycleCauseNone {
			t.Fatalf("close trace = %+v", event)
		}
		err := channel.SendTerminal(context.Background(), framechannel.Frame("too-late"))
		if !errors.Is(err, ErrClosed) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRetired {
			t.Fatalf("terminal after close = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		gate.release()
		if err := <-closeResult; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRelayRetirementCauseIsMonotonicAcrossLinkAndChannel(t *testing.T) {
	t.Run("earlier natural channel retirement survives later link failure", func(t *testing.T) {
		link := newLink(context.Background(), newScriptedSocket(), false)
		channel := link.installFixed(relaySessionID(33))
		if err := channel.Close(); err != nil {
			t.Fatal(err)
		}

		laterFailure := errors.New("later unrelated link failure")
		link.stop(laterFailure)
		if channel.Err() != nil {
			t.Fatalf("later link failure rewrote natural retirement: %v", channel.Err())
		}
		err := channel.Send(context.Background(), framechannel.Frame("stale-snapshot"))
		if !errors.Is(err, ErrClosed) || errors.Is(err, laterFailure) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRetired {
			t.Fatalf("stale send = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if !errors.Is(link.Err(), laterFailure) {
			t.Fatalf("link error = %v", link.Err())
		}
	})

	t.Run("deferred natural retirement keeps identity while later failure drains exactly", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleSendAdmitted)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(37))

		terminalResult := make(chan error, 1)
		go func() {
			terminalResult <- channel.SendTerminal(context.Background(), framechannel.Frame("terminal"))
		}()
		gate.wait(t)
		link.retire(channel.id)
		gate.waitFor(t, LifecycleRetirementDeferred)

		laterFailure := errors.New("failure after natural retirement authority")
		link.stop(laterFailure)
		gate.release()
		err := <-terminalResult
		if !errors.Is(err, laterFailure) ||
			framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
			t.Fatalf("accepted terminal drain = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if !errors.Is(channel.Err(), ErrSessionRetired) || errors.Is(channel.Err(), laterFailure) {
			t.Fatalf("later failure rewrote retirement identity: %v", channel.Err())
		}
		if !errors.Is(link.Err(), laterFailure) {
			t.Fatalf("link error = %v", link.Err())
		}
	})

	t.Run("earlier natural link retirement survives later link failure", func(t *testing.T) {
		link := newLink(context.Background(), newScriptedSocket(), false)
		channel := link.installFixed(relaySessionID(38))
		link.stop(nil)
		link.stop(errors.New("failure after link close"))
		if link.Err() != nil || channel.Err() != nil {
			t.Fatalf("natural link/channel retirement rewritten: link=%v channel=%v", link.Err(), channel.Err())
		}
		err := channel.Send(context.Background(), framechannel.Frame("stale"))
		if framechannel.SendDispositionOf(err) != framechannel.SendRetired {
			t.Fatalf("stale send = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
	})

	t.Run("published link failure wins before local close and drains its exact cause", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleLinkRetiring)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(34))

		sendResult := make(chan error, 1)
		go func() {
			sendResult <- channel.Send(context.Background(), framechannel.Frame("owned"))
		}()
		requireQueuedRequests(t, link, channel.id, 1)

		linkFailure := errors.New("authoritative link failure")
		stopResult := make(chan struct{})
		go func() {
			link.stop(linkFailure)
			close(stopResult)
		}()
		event := gate.wait(t)
		if event.RetirementSource != LifecycleRetirementLinkFailure ||
			event.Cause != LifecycleCauseTransport || event.DrainCause != LifecycleCauseTransport {
			t.Fatalf("link retirement trace = %+v", event)
		}

		if err := channel.Close(); err != nil {
			t.Fatal(err)
		}
		sendErr := <-sendResult
		if !errors.Is(sendErr, linkFailure) ||
			framechannel.SendDispositionOf(sendErr) != framechannel.SendAccepted {
			t.Fatalf("drained send = %v disposition=%d", sendErr, framechannel.SendDispositionOf(sendErr))
		}
		if !errors.Is(channel.Err(), linkFailure) {
			t.Fatalf("channel lost published link cause: %v", channel.Err())
		}
		gate.release()
		<-stopResult
		if !errors.Is(link.Err(), linkFailure) {
			t.Fatalf("link error = %v", link.Err())
		}
	})
}

func TestRelayCancellationAndAdmissionHaveOneLinearization(t *testing.T) {
	t.Run("completed cancellation wins before natural retirement admission", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleTerminalReserved)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(35))
		ctx, cancel := context.WithCancel(context.Background())

		terminalResult := make(chan error, 1)
		go func() {
			terminalResult <- channel.SendTerminal(ctx, framechannel.Frame("terminal"))
		}()
		gate.wait(t)
		cancel()
		<-ctx.Done()
		link.retire(channel.id)
		deferred := gate.waitFor(t, LifecycleRetirementDeferred)
		if deferred.RetirementSource != LifecycleRetirementRelaySession {
			t.Fatalf("retirement trace = %+v", deferred)
		}

		gate.release()
		err := <-terminalResult
		if !errors.Is(err, context.Canceled) ||
			framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatalf("pre-admission cancellation = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if channel.State() != framechannel.Closed || !errors.Is(channel.Err(), ErrSessionRetired) {
			t.Fatalf("post-rollback retirement state=%d error=%v", channel.State(), channel.Err())
		}
		requireQueuedRequests(t, link, channel.id, 0)
	})

	t.Run("irreversible admission keeps later cancellation accepted", func(t *testing.T) {
		gate := newLifecycleStageGate(LifecycleSendAdmitted)
		link := newLinkWithTracer(context.Background(), newScriptedSocket(), false, gate)
		channel := link.installFixed(relaySessionID(36))
		ctx, cancel := context.WithCancel(context.Background())

		terminalResult := make(chan error, 1)
		go func() {
			terminalResult <- channel.SendTerminal(ctx, framechannel.Frame("terminal"))
		}()
		event := gate.wait(t)
		if !event.Terminal || event.Disposition != framechannel.SendAccepted {
			t.Fatalf("admission trace = %+v", event)
		}
		request, ok := link.takeRequest()
		if !ok {
			t.Fatal("admitted terminal was not transport-owned")
		}
		link.retire(channel.id)
		gate.waitFor(t, LifecycleRetirementDeferred)
		cancel()
		<-ctx.Done()
		gate.release()

		err := <-terminalResult
		if !errors.Is(err, context.Canceled) ||
			framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
			t.Fatalf("post-admission cancellation = %v disposition=%d", err, framechannel.SendDispositionOf(err))
		}
		if channel.State() != framechannel.Closed || !errors.Is(channel.Err(), ErrSessionRetired) {
			t.Fatalf("authoritative retirement state=%d error=%v", channel.State(), channel.Err())
		}
		request.receipt <- nil
	})
}

func waitLifecycleTrace(
	t *testing.T,
	traces <-chan LifecycleTrace,
	stage LifecycleStage,
) LifecycleTrace {
	t.Helper()
	for {
		select {
		case event := <-traces:
			if event.Stage == stage {
				return event
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for lifecycle stage %q", stage)
			return LifecycleTrace{}
		}
	}
}

type lifecycleStageGate struct {
	stage   LifecycleStage
	events  chan LifecycleTrace
	entered chan LifecycleTrace
	unblock chan struct{}
	once    sync.Once
}

func newLifecycleStageGate(stage LifecycleStage) *lifecycleStageGate {
	return &lifecycleStageGate{
		stage: stage, events: make(chan LifecycleTrace, 64),
		entered: make(chan LifecycleTrace, 1), unblock: make(chan struct{}),
	}
}

func (gate *lifecycleStageGate) TraceRelayLifecycle(event LifecycleTrace) {
	gate.events <- event
	if event.Stage != gate.stage {
		return
	}
	gate.once.Do(func() {
		gate.entered <- event
		<-gate.unblock
	})
}

func (gate *lifecycleStageGate) wait(t *testing.T) LifecycleTrace {
	t.Helper()
	select {
	case event := <-gate.entered:
		return event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for gated lifecycle stage %q", gate.stage)
		return LifecycleTrace{}
	}
}

func (gate *lifecycleStageGate) waitFor(t *testing.T, stage LifecycleStage) LifecycleTrace {
	t.Helper()
	for {
		select {
		case event := <-gate.events:
			if event.Stage == stage {
				return event
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for lifecycle stage %q", stage)
			return LifecycleTrace{}
		}
	}
}

func (gate *lifecycleStageGate) release() {
	close(gate.unblock)
}

func requireQueuedRequests(t *testing.T, l *link, id v2.RelaySessionID, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		l.writeMu.Lock()
		got := 0
		if queue := l.queues[id]; queue != nil {
			got = len(queue.requests)
		}
		l.writeMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued requests = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func requireMutexHeld(t *testing.T, mutex *sync.Mutex) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for mutex.TryLock() {
		mutex.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("mutex was not acquired by the racing operation")
		}
		time.Sleep(time.Millisecond)
	}
}

type clientFixture struct {
	fresh      v2.RegisterInit
	token      v2.ResumeToken
	descriptor []byte
	privateKey ed25519.PrivateKey
}

func newClientFixture(t *testing.T) clientFixture {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x35}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	pkDigest := sha256.Sum256(append([]byte("windshare/v2 sender-key\x00"), publicKey...))
	var pkHash v2.PKHash
	copy(pkHash[:], pkDigest[:v2.PKHashBytes])
	shareDigest := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), pkHash[:]...))
	var shareID v2.ShareID
	copy(shareID[:], shareDigest[:v2.ShareIDBytes])
	var shareInstance v2.ShareInstance
	shareInstance[0] = 1
	var token v2.ResumeToken
	token[0] = 2
	descriptor := []byte("sealed descriptor object")
	fresh, err := NewFreshRegisterInit(shareID, shareInstance, pkHash, descriptor, token)
	if err != nil {
		t.Fatal(err)
	}
	return clientFixture{fresh: fresh, token: token, descriptor: descriptor, privateKey: privateKey}
}

func challengeFrame(t *testing.T, purpose v2.ChallengePurpose) []byte {
	t.Helper()
	var challenge v2.Challenge
	challenge.Purpose = purpose
	challenge.ID[0], challenge.Nonce[0] = 1, 2
	challenge.ExpiresAtUnixSeconds = uint64(time.Now().Add(time.Minute).Unix())
	encoded, err := challenge.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func relaySessionID(value byte) v2.RelaySessionID {
	var id v2.RelaySessionID
	id[0] = value
	return id
}

func assertWriteMagic(t *testing.T, socket *scriptedSocket, magic string) []byte {
	t.Helper()
	select {
	case message := <-socket.writes:
		if message.kind != websocket.MessageBinary || len(message.data) < 4 || string(message.data[:4]) != magic {
			t.Fatalf("write = %q, want %s", message.data, magic)
		}
		return message.data
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", magic)
		return nil
	}
}

func assertOpaqueWrite(t *testing.T, socket *scriptedSocket, id v2.RelaySessionID, want string) {
	t.Helper()
	encoded := assertWriteMagic(t, socket, "WS2O")
	route, err := v2.ParseOpaqueRoute(encoded)
	if err != nil || route.RelaySessionID != id || string(route.Ciphertext) != want {
		t.Fatalf("opaque route = %+v, %v", route, err)
	}
}

type scriptedMessage struct {
	kind websocket.MessageType
	data []byte
	err  error
}

type scriptedSocket struct {
	reads  chan scriptedMessage
	writes chan scriptedMessage
	done   chan struct{}
	once   sync.Once
	limit  atomic.Int64
}

func newScriptedSocket() *scriptedSocket {
	return &scriptedSocket{
		reads: make(chan scriptedMessage, 2_048), writes: make(chan scriptedMessage, 2_048), done: make(chan struct{}),
	}
}

func (socket *scriptedSocket) respond(data []byte) {
	socket.reads <- scriptedMessage{kind: websocket.MessageBinary, data: bytes.Clone(data)}
}

func (socket *scriptedSocket) dial(_ context.Context, _ string, header http.Header) (BinarySocket, error) {
	if value := header.Get("Origin"); value != "" && value != "https://app.example" {
		return nil, errors.New("unexpected origin")
	}
	return socket, nil
}

func (socket *scriptedSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-socket.done:
		return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
	case response := <-socket.reads:
		return response.kind, bytes.Clone(response.data), response.err
	}
}

func (socket *scriptedSocket) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-socket.done:
		return io.ErrClosedPipe
	case socket.writes <- scriptedMessage{kind: kind, data: bytes.Clone(data)}:
		return nil
	}
}

func (socket *scriptedSocket) Close(websocket.StatusCode, string) error {
	socket.once.Do(func() { close(socket.done) })
	return nil
}

func (socket *scriptedSocket) SetReadLimit(limit int64) { socket.limit.Store(limit) }
