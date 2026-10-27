package v2peer_test

import (
	"bytes"
	"context"
	"sync"

	"github.com/windshare/windshare/core/session/protocolsession"
)

// Negotiation fixtures retain session controls separately because they have no
// peer operation ID and must never enter an answer/candidate response queue.
type integrationSessionControls struct {
	mu          sync.Mutex
	pathHandler func(context.Context, []byte) error
	turnHandler func(context.Context, []byte) error
	sent        []receiverIntegrationControl
}

func (controls *integrationSessionControls) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	controls.mu.Lock()
	defer controls.mu.Unlock()
	controls.pathHandler = handler
}

func (controls *integrationSessionControls) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	controls.mu.Lock()
	defer controls.mu.Unlock()
	controls.turnHandler = handler
}

func (controls *integrationSessionControls) SendPeerPathControl(ctx context.Context, body []byte) error {
	return controls.send(ctx, protocolsession.MessagePeerPathControl, body)
}

func (controls *integrationSessionControls) SendTURNControl(ctx context.Context, body []byte) error {
	return controls.send(ctx, protocolsession.MessageTURNControl, body)
}

func (controls *integrationSessionControls) send(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := protocolsession.ValidateSessionControlBody(kind, body); err != nil {
		return err
	}
	controls.mu.Lock()
	defer controls.mu.Unlock()
	controls.sent = append(controls.sent, receiverIntegrationControl{kind: kind, body: bytes.Clone(body)})
	return nil
}
