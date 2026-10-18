package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type runtimeSender struct {
	*relayv2.SenderConnection
	wire *runtimeWire
}

func registerRuntime(endpoint string, f fixture, resume bool) runtimeSender {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	init := f.init.Registration
	if resume {
		init.Mode = v2.RegistrationResume
	}
	wire := newRuntimeWire()
	header := http.Header{}
	if secret := os.Getenv("WINDSHARE_RELAY_REGISTRATION_SECRET"); !resume && secret != "" {
		header.Set("Authorization", "Bearer "+secret)
	}
	sender, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: strings.TrimSuffix(endpoint, v2.WebSocketPath),
		Init:         init, SenderPrivateKey: f.key, Descriptor: f.descriptor, ResumeToken: f.token,
		Dial: relayv2.DialOptions{ReceiveCapacity: f.init.ReceiveCapacity, SocketDialer: wire.dial, Header: header},
	})
	check(err)
	return runtimeSender{sender, wire}
}

type runtimePair struct {
	sender   *relayv2.Channel
	receiver *relayv2.ReceiverConnection
	wire     *runtimeWire
}

func joinRuntime(ctx context.Context, endpoint string, f fixture, sender runtimeSender) *runtimePair {
	wire := newRuntimeWire()
	receiver, err := relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{
		RelayBaseURL: strings.TrimSuffix(endpoint, v2.WebSocketPath), ShareID: f.init.Registration.ShareID,
		Dial: relayv2.DialOptions{ReceiveCapacity: v2.ReceiveCapacity{Frames: 4, EncodedBytes: 2 * maximumEnvelopeBytes},
			SocketDialer: wire.dial},
	})
	check(err)
	require(bytes.Equal(receiver.Descriptor(), f.descriptor), "runtime JOIN descriptor changed")
	// The opaque bootstrap materializes the application channel. Real protocol
	// owners confirm admission only after authenticating their E2E handshake.
	bootstrap := []byte("runtime bootstrap")
	check(receiver.Channel().Send(ctx, bootstrap))
	channel, err := sender.Accept(ctx)
	check(err)
	require(channel.RelaySessionID() == receiver.Channel().RelaySessionID(), "runtime routed the wrong receiver")
	takeRuntime(ctx, channel, bootstrap)
	check(channel.ConfirmAdmission(ctx))
	return &runtimePair{sender: channel, receiver: receiver, wire: wire}
}

func takeRuntime(ctx context.Context, channel *relayv2.Channel, want []byte) {
	select {
	case frame, open := <-channel.Recv():
		require(open && bytes.Equal(frame, want), "runtime payload changed, reordered, or closed")
	case <-ctx.Done():
		panic("runtime receive stalled: " + ctx.Err().Error())
	}
}

func (p *runtimePair) exchange(ctx context.Context, count uint64) {
	results := make(chan error, 2)
	for side, channel := range []*relayv2.Channel{p.sender, p.receiver.Channel()} {
		go func(side byte, channel *relayv2.Channel) {
			for ordinal := range count {
				if err := channel.Send(ctx, ciphertext(side, ordinal)); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}(byte(side), channel)
	}
	read := make(chan struct{}, 2)
	for side, channel := range []*relayv2.Channel{p.sender, p.receiver.Channel()} {
		go func(side byte, channel *relayv2.Channel) {
			for ordinal := range count {
				takeRuntime(ctx, channel, ciphertext(1-side, ordinal))
			}
			read <- struct{}{}
		}(byte(side), channel)
	}
	for range 2 {
		check(<-results)
		<-read
	}
}

func (p *runtimePair) close() {
	check(p.receiver.Close())
	check(p.sender.Close())
}

func transferRuntime(endpoint string, f fixture, sender runtimeSender) *runtimePair {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()
	healthy := joinRuntime(ctx, endpoint, f, sender)
	healthy.exchange(ctx, transferFrames)
	// A one-frame tail must be reported even when no further application send
	// occurs to wake the writer.
	healthy.wire.until(ctx, healthy.sender.RelaySessionID(), func(s runtimeCounts) bool {
		return s.consumed.TotalFrames == s.received.TotalFrames && s.received.TotalFrames == transferFrames
	})
	slow := joinRuntime(ctx, endpoint, f, sender)
	defer slow.close()
	slowID := slow.sender.RelaySessionID()
	blocked := make(chan error, 1)
	go func() {
		for range 3 {
			if err := slow.sender.Send(ctx, bytes.Repeat([]byte{0xee}, v2.MaxOpaqueCiphertextBytes)); err != nil {
				blocked <- err
				return
			}
		}
		blocked <- nil
	}()
	slow.wire.until(ctx, slowID, func(s runtimeCounts) bool { return s.received.TotalFrames == 2 })
	// Also retain a prefix on the sender's shared connection, so cancellation
	// must settle its queue before those resources can serve healthy siblings.
	for range 2 {
		check(slow.receiver.Channel().Send(ctx, []byte("unconsumed request")))
	}
	sender.wire.until(ctx, slowID, func(s runtimeCounts) bool { return s.received.TotalFrames == 3 })
	healthy.exchange(ctx, transferFrames)
	select {
	case err := <-blocked:
		panic("slow consumer did not backpressure its sender: " + errorText(err))
	default:
	}
	check(slow.receiver.Close())
	sender.wire.until(ctx, slowID, func(s runtimeCounts) bool { return s.notice != nil })
	require(sender.wire.state(slowID).settled == nil, "runtime settled before application queue release")
	check(slow.sender.Close())
	sender.wire.until(ctx, slowID, func(s runtimeCounts) bool { return s.settled != nil })
	require(<-blocked != nil, "cancelled send succeeded")
	healthy.exchange(ctx, transferFrames)
	// STOP must not depend on consuming this final queued envelope.
	before := healthy.wire.state(healthy.sender.RelaySessionID()).received.TotalFrames
	check(healthy.sender.Send(ctx, []byte("submitted before STOP")))
	healthy.wire.until(ctx, healthy.sender.RelaySessionID(), func(s runtimeCounts) bool { return s.received.TotalFrames == before+1 })
	return healthy
}

func errorText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func stopRuntime(endpoint string, f fixture) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	check(relayv2.Stop(ctx, relayv2.StopConfig{
		RelayBaseURL: strings.TrimSuffix(endpoint, v2.WebSocketPath),
		ShareID:      f.init.Registration.ShareID, ShareInstance: f.init.Registration.ShareInstance,
		PKHash: f.init.Registration.PKHash, StopID: f.stopID, SenderPrivateKey: f.key,
	}))
}

func (p *runtimePair) stopped(sender runtimeSender) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	takeRuntime(ctx, p.receiver.Channel(), []byte("submitted before STOP"))
	for _, done := range []<-chan struct{}{sender.Done(), p.receiver.Done()} {
		select {
		case <-done:
		case <-ctx.Done():
			panic("STOP did not close runtime connections")
		}
	}
	require(errors.Is(p.sender.Err(), relayv2.ErrSessionRetired), "STOP did not retire the sender channel")
	require(p.wire.state(p.sender.RelaySessionID()).notice != nil, "STOP omitted receiver settlement notice")
}
