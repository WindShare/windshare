package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLaneAttachBodiesRejectAlternateSemanticsAndZeroNonces(t *testing.T) {
	requestBody, err := EncodeLaneAttachRequest(7)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeLaneAttachRequest(requestBody)
	if err != nil || request.requestedLaneID != 7 {
		t.Fatalf("request = %+v, %v", request, err)
	}
	hostile, _ := protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion, 1: laneAttachRequestMode, 2: uint64(7), 3: uint64(0),
	})
	if _, err := DecodeLaneAttachRequest(hostile); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("extra request field error = %v", err)
	}
	grant := protocolsession.LaneGrant{
		LaneID: 7, LaneEpoch: 3, OperationID: id16[protocolsession.OperationID](9),
	}
	if _, err := EncodeLaneGrant(grant); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("zero grant nonce error = %v", err)
	}
	copy(grant.AttachNonce[:], bytes.Repeat([]byte{1}, protocolsession.LaneAttachNonceBytes))
	grantBody, err := EncodeLaneGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLaneGrant(grantBody, grant.OperationID)
	if err != nil || decoded.LaneID != grant.LaneID || decoded.LaneEpoch != grant.LaneEpoch || decoded.AttachNonce != grant.AttachNonce {
		t.Fatalf("grant = %+v, %v", decoded, err)
	}
	readerBytes := append(make([]byte, protocolsession.LaneAttachNonceBytes), bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)...)
	nonce, err := ReadNonzeroLaneNonce(bytes.NewReader(readerBytes), protocolsession.LaneAttachNonceBytes)
	if err != nil || !bytes.Equal(nonce, bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)) {
		t.Fatalf("retried nonce = %x, %v", nonce, err)
	}
	if _, err := ReadNonzeroLaneNonce(bytes.NewReader(make([]byte, laneNonceAttempts)), 1); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("zero nonce source error = %v", err)
	}
}

type observedMemoryChannel struct {
	*memoryChannel
	sends          atomic.Int32
	terminals      atomic.Int32
	gateMu         sync.Mutex
	nextSendGate   *observedSendGate
	nextResultGate *observedSendGate
	loseAndClose   *atomic.Int32
}

type observedSendGate struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (gate *observedSendGate) wait(ctx context.Context) error {
	if gate == nil {
		return nil
	}
	close(gate.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate.release:
		return gate.err
	}
}

func (channel *observedMemoryChannel) gateNextSend() *observedSendGate {
	gate := &observedSendGate{started: make(chan struct{}), release: make(chan struct{})}
	channel.gateMu.Lock()
	channel.nextSendGate = gate
	channel.gateMu.Unlock()
	return gate
}

// Delivery and the transport's return value are separate events: a receiver can
// accept a frame while the sender still cannot determine whether it arrived.
func (channel *observedMemoryChannel) gateNextSendResult(err error) *observedSendGate {
	gate := &observedSendGate{started: make(chan struct{}), release: make(chan struct{}), err: err}
	channel.gateMu.Lock()
	channel.nextResultGate = gate
	channel.gateMu.Unlock()
	return gate
}

func (channel *observedMemoryChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	channel.gateMu.Lock()
	sendGate, resultGate := channel.nextSendGate, channel.nextResultGate
	loss := channel.loseAndClose
	channel.nextSendGate, channel.nextResultGate = nil, nil
	channel.gateMu.Unlock()
	if loss != nil {
		if loss.Add(1) == 1 {
			return nil
		}
		return errors.New("accepted frame lost when carrier closed")
	}
	if err := sendGate.wait(ctx); err != nil {
		return err
	}
	if err := channel.memoryChannel.Send(ctx, frame); err != nil {
		return err
	}
	return resultGate.wait(ctx)
}

func (channel *observedMemoryChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	channel.terminals.Add(1)
	return channel.memoryChannel.Send(ctx, frame)
}
