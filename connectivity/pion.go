package connectivity

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	// DefaultSTUNServer provides server-reflexive candidates without coupling
	// TURN or relay-data policy to the WebRTC transport adapter.
	DefaultSTUNServer = "stun:stun.l.google.com:19302"

	// maxCandidatesPerPeer bounds both directions for the lifetime of one ICE
	// gathering. The accepted browser/Pion spike observes at most ten; the wider
	// ceiling tolerates multi-interface hosts without granting a signaling peer
	// an unbounded allocation surface inside Pion.
	maxCandidatesPerPeer  = 64
	inboundSignalCapacity = 32
	failureQueueCapacity  = 8
)

// PeerChannel exposes the lifecycle signals negotiation needs in addition to
// the transport-neutral FrameChannel consumed by core/session.
type PeerChannel interface {
	session.FrameChannel
	Opened() <-chan struct{}
	Done() <-chan struct{}
	Err() error
}

// OfferChannelFactory is defined on the receiver side that consumes it.
type OfferChannelFactory interface {
	Offer(context.Context, Signaling) (PeerChannel, error)
}

// AnswerChannelFactory is defined on the sender side that consumes it.
type AnswerChannelFactory interface {
	Answer(context.Context, Signaling) (PeerChannel, error)
}

// PionChannelFactory creates one independently owned PeerConnection per relay
// session. It performs signaling, then returns only after the DataChannel is
// Open; the returned channel still outlives a later loss of relay signaling.
type PionChannelFactory struct {
	configuration pion.Configuration
	newPeer       func(pion.Configuration) (*pion.PeerConnection, error)
}

func DefaultPionConfiguration() pion.Configuration {
	return pion.Configuration{
		ICEServers: []pion.ICEServer{{URLs: []string{DefaultSTUNServer}}},
	}
}

func NewPionChannelFactory(configuration pion.Configuration) *PionChannelFactory {
	return &PionChannelFactory{
		configuration: clonePionConfiguration(configuration),
		newPeer:       pion.NewPeerConnection,
	}
}

func (f *PionChannelFactory) Offer(ctx context.Context, signaling Signaling) (PeerChannel, error) {
	return f.negotiate(ctx, signaling, negotiationOfferer)
}

func (f *PionChannelFactory) Answer(ctx context.Context, signaling Signaling) (PeerChannel, error) {
	return f.negotiate(ctx, signaling, negotiationAnswerer)
}

type negotiationRole uint8

const (
	negotiationOfferer negotiationRole = iota
	negotiationAnswerer
)

type negotiationFailure struct {
	err                error
	fatalAfterOpen     bool
	inboundUnavailable bool
}

type pionNegotiation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	signaling Signaling
	peer      *pion.PeerConnection

	inbound         chan Signal
	candidates      chan Signal
	failures        chan negotiationFailure
	descriptionSent chan struct{}
	descriptionOnce sync.Once

	localCandidates    atomic.Uint32
	remoteDataChannels atomic.Uint32
}

// admissionState carries the mutable pre-Open negotiation state shared by the
// phase handlers: channel ownership, remote-description gating, and the
// per-peer candidate budget.
type admissionState struct {
	role                 negotiationRole
	channel              *ownedPeerChannel
	remoteDescriptionSet bool
	remoteCandidateCount int
	pendingCandidates    []pion.ICECandidateInit
}

func (f *PionChannelFactory) negotiate(
	ctx context.Context,
	signaling Signaling,
	role negotiationRole,
) (PeerChannel, error) {
	if signaling == nil {
		return nil, fmt.Errorf("%w: signaling", ErrNilDependency)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	peer, err := f.newPeer(clonePionConfiguration(f.configuration))
	if err != nil {
		return nil, fmt.Errorf("%w: create PeerConnection: %w", ErrPeerConnectionFailed, err)
	}

	negotiationCtx, cancel := context.WithCancel(ctx)
	n := &pionNegotiation{
		ctx:             negotiationCtx,
		cancel:          cancel,
		signaling:       signaling,
		peer:            peer,
		inbound:         make(chan Signal, inboundSignalCapacity),
		candidates:      make(chan Signal, maxCandidatesPerPeer),
		failures:        make(chan negotiationFailure, failureQueueCapacity),
		descriptionSent: make(chan struct{}),
	}
	channelResults := make(chan dataChannelResult, 1)
	n.configureRemoteDataChannels(role, channelResults)
	n.start()

	state := &admissionState{
		role:              role,
		pendingCandidates: make([]pion.ICECandidateInit, 0, maxCandidatesPerPeer),
	}
	if role == negotiationOfferer {
		if err := n.openOffererChannel(state); err != nil {
			n.abort()
			return nil, err
		}
	}
	channel, err := n.awaitAdmission(ctx, state, channelResults)
	if err != nil {
		n.abort()
		return nil, err
	}
	return channel, nil
}

func (n *pionNegotiation) openOffererChannel(state *admissionState) error {
	transportChannel, err := createOffererChannel(n.peer)
	if err != nil {
		return err
	}
	state.channel = n.own(transportChannel)
	return n.createAndSendOffer()
}

// awaitAdmission drives signaling until the DataChannel is admitted as Open or
// the negotiation fails. On success ownership of the negotiation context moves
// to maintain; on error the caller aborts, which closes the PeerConnection.
func (n *pionNegotiation) awaitAdmission(
	ctx context.Context,
	state *admissionState,
	channelResults <-chan dataChannelResult,
) (*ownedPeerChannel, error) {
	for {
		var opened <-chan struct{}
		var channelDone <-chan struct{}
		if state.channel != nil {
			opened = state.channel.Opened()
			channelDone = state.channel.Done()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case failure := <-n.failures:
			return n.settleFailure(ctx, state, failure, channelResults)
		case result := <-channelResults:
			if result.err != nil {
				return nil, fmt.Errorf("%w: wrap remote DataChannel: %w", ErrPeerConnectionFailed, result.err)
			}
			state.channel = result.channel
		case <-channelDone:
			reason := state.channel.Err()
			if reason == nil {
				reason = ErrPeerConnectionFailed
			}
			return nil, fmt.Errorf("%w: DataChannel closed before Open: %w", ErrPeerConnectionFailed, reason)
		case signal := <-n.inbound:
			if err := n.handleAdmissionSignal(state, signal); err != nil {
				return nil, err
			}
		case <-opened:
			return n.admitOpenedChannel(ctx, state)
		}
	}
}

// settleFailure decides whether an already-open channel survives a reported
// failure. OnDataChannel precedes OnOpen, but its callback result and a relay
// read failure can become ready together, so the pending channel result is
// reconciled before deciding whether the channel can survive signaling loss.
func (n *pionNegotiation) settleFailure(
	ctx context.Context,
	state *admissionState,
	failure negotiationFailure,
	channelResults <-chan dataChannelResult,
) (*ownedPeerChannel, error) {
	if state.channel == nil && state.role == negotiationAnswerer {
		select {
		case result := <-channelResults:
			if result.err != nil {
				return nil, fmt.Errorf("%w: wrap remote DataChannel: %w", ErrPeerConnectionFailed, result.err)
			}
			state.channel = result.channel
		default:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.channel != nil && state.channel.State() == session.Open && !failure.fatalAfterOpen {
		go n.maintain(state.channel, !failure.inboundUnavailable, state.remoteCandidateCount)
		return state.channel, nil
	}
	return nil, failure.err
}

func (n *pionNegotiation) handleAdmissionSignal(state *admissionState, signal Signal) error {
	switch signal.Kind {
	case protocol.SignalKindCandidate:
		return n.admitRemoteCandidate(state, signal.Payload)
	case protocol.SignalKindOffer, protocol.SignalKindAnswer:
		return n.applyRemoteDescription(state, signal)
	default:
		return fmt.Errorf("%w: kind %q", ErrUnexpectedSignal, signal.Kind)
	}
}

// admitRemoteCandidate buffers candidates that arrive ahead of the remote
// description; Pion rejects AddICECandidate until one is set.
func (n *pionNegotiation) admitRemoteCandidate(state *admissionState, payload json.RawMessage) error {
	if state.remoteCandidateCount == maxCandidatesPerPeer {
		return ErrCandidateLimitExceeded
	}
	candidate, err := decodeCandidate(payload)
	if err != nil {
		return err
	}
	state.remoteCandidateCount++
	if !state.remoteDescriptionSet {
		state.pendingCandidates = append(state.pendingCandidates, candidate)
		return nil
	}
	if err := n.peer.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("%w: add ICE candidate: %w", ErrInvalidSignal, err)
	}
	return nil
}

func (n *pionNegotiation) applyRemoteDescription(state *admissionState, signal Signal) error {
	expectedKind, expectedType := expectedRemoteDescription(state.role)
	if signal.Kind != expectedKind || state.remoteDescriptionSet {
		return fmt.Errorf("%w: got %q", ErrUnexpectedSignal, signal.Kind)
	}
	description, err := decodeDescription(signal.Payload, expectedType)
	if err != nil {
		return err
	}
	if err := n.peer.SetRemoteDescription(description); err != nil {
		return fmt.Errorf("%w: set remote description: %w", ErrInvalidSignal, err)
	}
	state.remoteDescriptionSet = true
	if err := flushCandidates(n.peer, state.pendingCandidates); err != nil {
		return err
	}
	state.pendingCandidates = nil
	if state.role == negotiationAnswerer {
		return n.createAndSendAnswer()
	}
	return nil
}

func (n *pionNegotiation) admitOpenedChannel(ctx context.Context, state *admissionState) (*ownedPeerChannel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.channel.State() != session.Open {
		reason := state.channel.Err()
		if reason == nil {
			reason = ErrPeerConnectionFailed
		}
		return nil, fmt.Errorf("%w: DataChannel left Open before admission: %w", ErrPeerConnectionFailed, reason)
	}
	go n.maintain(state.channel, true, state.remoteCandidateCount)
	return state.channel, nil
}

func createOffererChannel(peer *pion.PeerConnection) (PeerChannel, error) {
	dataChannel, err := peer.CreateDataChannel(
		transportwebrtc.ChannelLabel,
		transportwebrtc.DefaultDataChannelInit(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create DataChannel: %w", ErrPeerConnectionFailed, err)
	}
	channel, err := transportwebrtc.NewChannel(dataChannel)
	if err != nil {
		_ = dataChannel.Close()
		return nil, fmt.Errorf("%w: wrap local DataChannel: %w", ErrPeerConnectionFailed, err)
	}
	return channel, nil
}

func (n *pionNegotiation) start() {
	n.peer.OnICECandidate(n.onICECandidate)
	n.peer.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateFailed {
			n.report(negotiationFailure{
				err:            ErrPeerConnectionFailed,
				fatalAfterOpen: true,
			})
		}
	})
	go n.readSignals()
	go n.writeCandidates()
}

func (n *pionNegotiation) onICECandidate(candidate *pion.ICECandidate) {
	if candidate == nil {
		return
	}
	if n.localCandidates.Add(1) > maxCandidatesPerPeer {
		n.report(negotiationFailure{err: ErrCandidateLimitExceeded, fatalAfterOpen: true})
		return
	}
	payload, err := json.Marshal(candidate.ToJSON())
	if err != nil {
		n.report(negotiationFailure{err: fmt.Errorf("%w: encode ICE candidate: %w", ErrInvalidSignal, err), fatalAfterOpen: true})
		return
	}
	signal := Signal{Kind: protocol.SignalKindCandidate, Payload: payload}
	select {
	case <-n.ctx.Done():
	case n.candidates <- signal:
	default:
		n.report(negotiationFailure{err: ErrCandidateLimitExceeded, fatalAfterOpen: true})
	}
}

func (n *pionNegotiation) readSignals() {
	for {
		signal, err := n.signaling.Receive(n.ctx)
		if err != nil {
			if n.ctx.Err() == nil {
				n.report(negotiationFailure{
					err:                fmt.Errorf("connectivity: receive signal: %w", err),
					inboundUnavailable: true,
				})
			}
			return
		}
		select {
		case <-n.ctx.Done():
			return
		case n.inbound <- signal:
		}
	}
}

func (n *pionNegotiation) writeCandidates() {
	select {
	case <-n.ctx.Done():
		return
	case <-n.descriptionSent:
	}
	for {
		select {
		case <-n.ctx.Done():
			return
		case candidate := <-n.candidates:
			if err := n.signaling.Send(n.ctx, candidate); err != nil {
				if n.ctx.Err() == nil {
					n.report(negotiationFailure{err: fmt.Errorf("connectivity: send ICE candidate: %w", err)})
				}
				return
			}
		}
	}
}

func (n *pionNegotiation) createAndSendOffer() error {
	offer, err := n.peer.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("%w: create offer: %w", ErrPeerConnectionFailed, err)
	}
	return n.setLocalAndSend(protocol.SignalKindOffer, offer)
}

func (n *pionNegotiation) createAndSendAnswer() error {
	answer, err := n.peer.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("%w: create answer: %w", ErrPeerConnectionFailed, err)
	}
	return n.setLocalAndSend(protocol.SignalKindAnswer, answer)
}

func (n *pionNegotiation) setLocalAndSend(kind string, description pion.SessionDescription) error {
	if err := n.peer.SetLocalDescription(description); err != nil {
		return fmt.Errorf("%w: set local description: %w", ErrPeerConnectionFailed, err)
	}
	local := n.peer.LocalDescription()
	if local == nil {
		return fmt.Errorf("%w: local description is unavailable", ErrPeerConnectionFailed)
	}
	payload, err := json.Marshal(local)
	if err != nil {
		return fmt.Errorf("%w: encode local description: %w", ErrInvalidSignal, err)
	}
	if err := n.signaling.Send(n.ctx, Signal{Kind: kind, Payload: payload}); err != nil {
		return fmt.Errorf("connectivity: send %s: %w", kind, err)
	}
	n.descriptionOnce.Do(func() { close(n.descriptionSent) })
	return nil
}

func (n *pionNegotiation) maintain(
	channel *ownedPeerChannel,
	inboundAvailable bool,
	remoteCandidateCount int,
) {
	defer n.cancel()
	aborted := false
	defer func() {
		if aborted {
			// Fatal cleanup must never wait behind an admitted terminal before the
			// parent is gone. peer.Close is idempotent, so repeat it here to keep
			// the ordering explicit even when the abort path already closed it.
			_ = n.peer.Close()
			_ = channel.Close()
			return
		}
		// A clean owner Close or acknowledged terminal keeps D1's channel-first
		// ordering so SCTP can drain the terminal acknowledgement.
		_ = channel.Close()
		_ = n.peer.Close()
	}()
	abort := func(err error) {
		aborted = true
		channel.fail(err)
		// Close the parent synchronously before channel cleanup. D1 deliberately
		// lets Close wait behind a locally admitted terminal; without this abort,
		// a hostile peer withholding its ACK could pin orchestration forever.
		_ = n.peer.Close()
	}
	var inbound <-chan Signal
	if inboundAvailable {
		inbound = n.inbound
	}
	channelDone := channel.Done()
	for {
		select {
		case <-n.ctx.Done():
			if !channel.releasedByOwner() {
				abort(n.ctx.Err())
			}
			return
		case <-channelDone:
			// Done is a logical boundary. For a remotely initiated terminal it
			// becomes observable immediately after the ack enters SCTP; closing
			// the PeerConnection here can still overtake that ack on the wire.
			channelDone = nil
			if channel.Err() != nil {
				aborted = true
				_ = n.peer.Close()
				return
			}
		case failure := <-n.failures:
			if failure.fatalAfterOpen {
				abort(failure.err)
				return
			}
			if failure.inboundUnavailable {
				// Once the DataChannel is Open, relay loss removes only the
				// signaling path; the established P2P data path remains valid.
				inbound = nil
			}
		case signal := <-inbound:
			if signal.Kind != protocol.SignalKindCandidate {
				abort(fmt.Errorf("%w: kind %q after channel Open", ErrUnexpectedSignal, signal.Kind))
				return
			}
			if remoteCandidateCount == maxCandidatesPerPeer {
				abort(ErrCandidateLimitExceeded)
				return
			}
			candidate, err := decodeCandidate(signal.Payload)
			if err != nil {
				abort(err)
				return
			}
			remoteCandidateCount++
			if err := n.peer.AddICECandidate(candidate); err != nil {
				abort(fmt.Errorf("%w: add ICE candidate after channel Open: %w", ErrInvalidSignal, err))
				return
			}
		}
	}
}

func (n *pionNegotiation) report(failure negotiationFailure) {
	select {
	case <-n.ctx.Done():
	case n.failures <- failure:
	default:
		// A full queue already contains an earlier failure that will settle the
		// same negotiation. Pion callbacks must never block behind diagnostics.
	}
}

func (n *pionNegotiation) abort() {
	n.cancel()
	_ = n.peer.Close()
}

func expectedRemoteDescription(role negotiationRole) (string, pion.SDPType) {
	if role == negotiationOfferer {
		return protocol.SignalKindAnswer, pion.SDPTypeAnswer
	}
	return protocol.SignalKindOffer, pion.SDPTypeOffer
}

func decodeDescription(payload json.RawMessage, expected pion.SDPType) (pion.SessionDescription, error) {
	if !isJSONObject(payload) {
		return pion.SessionDescription{}, fmt.Errorf("%w: session description must be a JSON object", ErrInvalidSignal)
	}
	var description pion.SessionDescription
	if err := json.Unmarshal(payload, &description); err != nil {
		return pion.SessionDescription{}, fmt.Errorf("%w: decode session description: %w", ErrInvalidSignal, err)
	}
	if description.Type != expected || description.SDP == "" {
		return pion.SessionDescription{}, fmt.Errorf("%w: session description type %q", ErrUnexpectedSignal, description.Type.String())
	}
	return description, nil
}

func decodeCandidate(payload json.RawMessage) (pion.ICECandidateInit, error) {
	if !isJSONObject(payload) {
		return pion.ICECandidateInit{}, fmt.Errorf("%w: ICE candidate must be a JSON object", ErrInvalidSignal)
	}
	var candidate pion.ICECandidateInit
	if err := json.Unmarshal(payload, &candidate); err != nil {
		return pion.ICECandidateInit{}, fmt.Errorf("%w: decode ICE candidate: %w", ErrInvalidSignal, err)
	}
	return candidate, nil
}

func flushCandidates(peer *pion.PeerConnection, candidates []pion.ICECandidateInit) error {
	for _, candidate := range candidates {
		if err := peer.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("%w: add buffered ICE candidate: %w", ErrInvalidSignal, err)
		}
	}
	return nil
}

func isJSONObject(payload json.RawMessage) bool {
	for _, b := range payload {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b == '{'
		}
	}
	return false
}

func clonePionConfiguration(configuration pion.Configuration) pion.Configuration {
	clone := configuration
	clone.ICEServers = append([]pion.ICEServer(nil), configuration.ICEServers...)
	for i := range clone.ICEServers {
		clone.ICEServers[i].URLs = append([]string(nil), configuration.ICEServers[i].URLs...)
	}
	clone.Certificates = append([]pion.Certificate(nil), configuration.Certificates...)
	return clone
}

var (
	_ OfferChannelFactory  = (*PionChannelFactory)(nil)
	_ AnswerChannelFactory = (*PionChannelFactory)(nil)
)
