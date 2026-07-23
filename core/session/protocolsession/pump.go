package protocolsession

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

var (
	ErrPumpReused            = errors.New("protocolsession: pump Run may only be called once")
	ErrEnvelopeRejected      = errors.New("protocolsession: inbound envelope rejected")
	ErrInboundAuthentication = errors.New("protocolsession: inbound message authentication failed")
	ErrInboundRejected       = errors.New("protocolsession: inbound message rejected")
	ErrPeerSessionTerminal   = errors.New("protocolsession: peer ended the session")
)

type InboundEnvelopeOpener interface {
	Open(frame []byte) (OpenedEnvelope, error)
}

// InboundMessageAuthenticator verifies authority that AEAD alone cannot grant.
// In particular, a capability holder knows traffic keys but cannot forge sender
// control signatures. It runs before operation state or dispatch is touched.
type InboundMessageAuthenticator interface {
	AuthenticateInbound(sequence uint64, message Message) (InboundAuthenticationResult, error)
}

type InboundMessageAuthenticatorFunc func(
	sequence uint64,
	message Message,
) (InboundAuthenticationResult, error)

func (authenticate InboundMessageAuthenticatorFunc) AuthenticateInbound(
	sequence uint64,
	message Message,
) (InboundAuthenticationResult, error) {
	if authenticate == nil {
		return InboundAuthenticationResult{}, ErrNilRuntimeDependency
	}
	return authenticate(sequence, message)
}

// PeerSessionTerminalError retains the authenticated message for a caller that
// needs to decode a diagnostic while still supporting errors.Is classification.
type PeerSessionTerminalError struct {
	message Message
}

func (terminal *PeerSessionTerminalError) Error() string { return ErrPeerSessionTerminal.Error() }
func (terminal *PeerSessionTerminalError) Unwrap() error { return ErrPeerSessionTerminal }
func (terminal *PeerSessionTerminalError) Message() Message {
	return terminal.message
}

type ProtocolPump struct {
	channel       FrameChannel
	opener        InboundEnvelopeOpener
	authenticator InboundMessageAuthenticator
	router        InboundMessageRouter

	started atomic.Bool
}

func NewProtocolPump(
	channel FrameChannel,
	opener InboundEnvelopeOpener,
	authenticator InboundMessageAuthenticator,
	router InboundMessageRouter,
) (*ProtocolPump, error) {
	if channel == nil || opener == nil || authenticator == nil || router == nil {
		return nil, ErrNilRuntimeDependency
	}
	return &ProtocolPump{
		channel: channel, opener: opener, authenticator: authenticator, router: router,
	}, nil
}

// Run acquires Recv exactly once and never exposes it to business code. The
// injected opener owns replay/sequence validation before any message allocation
// or operation transition is admitted.
func (pump *ProtocolPump) Run(ctx context.Context) error {
	if !pump.started.CompareAndSwap(false, true) {
		return ErrPumpReused
	}
	receive := pump.channel.Recv()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-receive:
			if !ok {
				return nil
			}
			if err := pump.acceptFrame(ctx, frame); err != nil {
				return err
			}
		}
	}
}

func (pump *ProtocolPump) acceptFrame(ctx context.Context, frame framechannel.Frame) error {
	opened, err := pump.opener.Open(frame)
	if err != nil {
		// Authentication failures remain local: reflecting an attacker-controlled
		// operation identity would turn the terminal path into an oracle.
		return pump.rejectLocally(fmt.Errorf("%w: %w", ErrEnvelopeRejected, err))
	}
	message, err := DecodeMessage(opened.Plaintext)
	if err != nil {
		return pump.rejectLocally(fmt.Errorf("%w: %w", ErrInboundRejected, err))
	}
	authentication, err := pump.authenticator.AuthenticateInbound(opened.Sequence, message)
	if err != nil {
		return pump.rejectLocally(fmt.Errorf("%w: %w", ErrInboundAuthentication, err))
	}
	if authentication.operationViolation.valid() {
		bound, routeErr := pump.router.RouteAuthenticatedOperationViolation(
			ctx,
			message,
			authentication.operationViolation,
		)
		cause := fmt.Errorf(
			"%w: code=%d bound=%t",
			ErrAuthenticatedOperationViolation,
			authentication.operationViolation.code,
			bound,
		)
		if routeErr != nil {
			cause = fmt.Errorf("%w: route violation: %w", cause, routeErr)
		}
		if authentication.operationViolation.code == AuthenticatedOperationViolationMalformedPeerControl &&
			bound && routeErr == nil {
			return nil
		}
		return pump.rejectLocally(cause)
	}
	disposition, err := pump.router.RouteInbound(ctx, message)
	if err != nil {
		return pump.rejectLocally(fmt.Errorf("%w: %w", ErrInboundRejected, err))
	}
	if disposition == OperationSessionTerminal {
		semantic, semanticErr := SenderControlSemanticBody(message)
		if semanticErr == nil {
			_, semanticErr = DecodeSessionTerminal(semantic)
		}
		if semanticErr != nil {
			return pump.rejectLocally(fmt.Errorf("%w: %w", ErrInboundRejected, semanticErr))
		}
		return &PeerSessionTerminalError{message: message}
	}
	return nil
}

func (pump *ProtocolPump) rejectLocally(cause error) error {
	if err := pump.router.TerminateLocal(); err != nil && !errors.Is(err, ErrSessionTerminated) {
		return errors.Join(cause, fmt.Errorf("terminate rejected protocol session: %w", err))
	}
	return cause
}
