package browsermatrixpion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

const (
	attemptStatePending     = "pending"
	attemptStateEstablished = "established"
	attemptStateFailed      = "failed"
	protocolUDP             = "udp"
)

// Attempt is the lease-owned unit of Pion state. The service consumes this
// narrow contract so lease and containment behavior can be tested without
// teaching HTTP tests about Pion internals.
type Attempt interface {
	Offer(context.Context, string) (string, error)
	Result() AttemptResult
	Close() error
}

type AttemptFactory interface {
	Create(context.Context, AttemptAuthority) (Attempt, error)
}

type PionAttemptFactoryConfig struct {
	InstanceID string
	PublicIP   string
	UDPPortMin uint16
	UDPPortMax uint16
	Trace      TraceSink
}

type PionAttemptFactory struct {
	api        *pion.API
	instanceID string
	trace      TraceSink
}

func NewPionAttemptFactory(config PionAttemptFactoryConfig) (*PionAttemptFactory, error) {
	if err := validateCanonicalID(config.InstanceID, "instance ID"); err != nil {
		return nil, err
	}
	address, err := netip.ParseAddr(config.PublicIP)
	if err != nil || !address.Is4() || address.String() != config.PublicIP || config.UDPPortMin == 0 || config.UDPPortMax < config.UDPPortMin {
		return nil, errors.New("remote Pion endpoint authority is invalid")
	}
	var setting pion.SettingEngine
	setting.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	if err := setting.SetEphemeralUDPPortRange(config.UDPPortMin, config.UDPPortMax); err != nil {
		return nil, fmt.Errorf("bind remote Pion UDP authority: %w", err)
	}
	// The configured address is operator-authorized public routing state. Pion
	// must advertise that address rather than an incidental interface address
	// enumerated by the host at runtime.
	if err := setting.SetICEAddressRewriteRules(pion.ICEAddressRewriteRule{
		External:        []string{config.PublicIP},
		AsCandidateType: pion.ICECandidateTypeHost,
		Mode:            pion.ICEAddressRewriteReplace,
	}); err != nil {
		return nil, fmt.Errorf("configure remote Pion address rewrite: %w", err)
	}
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return &PionAttemptFactory{
		api: pion.NewAPI(pion.WithSettingEngine(setting)), instanceID: config.InstanceID, trace: config.Trace,
	}, nil
}

func (factory *PionAttemptFactory) Create(ctx context.Context, authority AttemptAuthority) (Attempt, error) {
	if factory == nil || factory.api == nil || ValidateAttemptAuthority(authority) != nil ||
		authority.RequestAuthority.FixtureBinding.RemoteServiceInstanceID != factory.instanceID {
		return nil, errors.New("remote Pion attempt authority is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("remote Pion attempt creation deadline exceeded")
	}
	peer, err := factory.api.NewPeerConnection(pion.Configuration{
		ICEServers:         []pion.ICEServer{},
		ICETransportPolicy: pion.ICETransportPolicyAll,
	})
	if err != nil {
		return nil, fmt.Errorf("create remote Pion peer: %w", err)
	}
	attempt := &pionAttempt{
		authority: authority, peer: peer,
		instanceID: factory.instanceID, trace: factory.trace,
		result: AttemptResult{
			ProtocolVersion: ProtocolVersion, AttemptAuthority: authority,
			State: attemptStatePending, ChallengeBindingSHA256: challengeBindingSHA256(authority),
		},
	}
	peer.OnDataChannel(func(channel *pion.DataChannel) {
		channel.OnMessage(func(message pion.DataChannelMessage) { attempt.onChallenge(channel, message) })
	})
	peer.OnICEConnectionStateChange(attempt.onICEState)
	return attempt, nil
}

type pionAttempt struct {
	authority         AttemptAuthority
	instanceID        string
	trace             TraceSink
	peer              *pion.PeerConnection
	mu                sync.RWMutex
	result            AttemptResult
	offerUsed         bool
	closeOnce         sync.Once
	closeErr          error
	challengeReceived bool
	applicationProven bool
}

type challengeEchoer interface {
	SendText(string) error
}

func (attempt *pionAttempt) Offer(ctx context.Context, offerSDP string) (string, error) {
	attempt.mu.Lock()
	if attempt.offerUsed {
		attempt.mu.Unlock()
		return "", errors.New("remote Pion attempt already consumed its offer")
	}
	attempt.offerUsed = true
	attempt.mu.Unlock()

	if err := attempt.peer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: offerSDP}); err != nil {
		attempt.fail("invalid-offer")
		return "", errors.New("remote Pion rejected the offer")
	}
	answer, err := attempt.peer.CreateAnswer(nil)
	if err != nil {
		attempt.fail("answer-creation-failed")
		return "", errors.New("remote Pion could not create an answer")
	}
	gatheringComplete := pion.GatheringCompletePromise(attempt.peer)
	if err := attempt.peer.SetLocalDescription(answer); err != nil {
		attempt.fail("local-description-failed")
		return "", errors.New("remote Pion could not bind its answer")
	}
	select {
	case <-ctx.Done():
		attempt.fail("offer-deadline-exceeded")
		return "", errors.New("remote Pion offer deadline exceeded")
	case <-gatheringComplete:
	}
	local := attempt.peer.LocalDescription()
	if local == nil || local.SDP == "" {
		attempt.fail("answer-unavailable")
		return "", errors.New("remote Pion answer is unavailable")
	}
	return local.SDP, nil
}

func (attempt *pionAttempt) Result() AttemptResult {
	attempt.mu.RLock()
	defer attempt.mu.RUnlock()
	result := attempt.result
	if attempt.result.SelectedPair != nil {
		pair := *attempt.result.SelectedPair
		result.SelectedPair = &pair
	}
	if attempt.result.FailureCode != nil {
		code := *attempt.result.FailureCode
		result.FailureCode = &code
	}
	return result
}

func (attempt *pionAttempt) Close() error {
	attempt.closeOnce.Do(func() {
		attempt.closeErr = attempt.peer.Close()
	})
	return attempt.closeErr
}

func (attempt *pionAttempt) onICEState(state pion.ICEConnectionState) {
	switch state {
	case pion.ICEConnectionStateConnected, pion.ICEConnectionStateCompleted:
		pair, err := selectedPair(attempt.peer)
		if err != nil {
			attempt.fail("selected-pair-unavailable")
			return
		}
		attempt.mu.Lock()
		attempt.result.SelectedPair = &pair
		attempt.establishIfProvenLocked()
		attempt.mu.Unlock()
		emitTrace(attempt.trace, TraceEvent{
			Milestone: tracePairObserved, InstanceID: attempt.instanceID,
			RunID:             attempt.authority.RequestAuthority.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: attempt.authority.RequestAuthority.FixtureBinding.AttestationSHA256,
			AttemptID:         attempt.authority.AttemptID, Outcome: "selected",
		})
	case pion.ICEConnectionStateFailed:
		attempt.fail("ice-failed")
	case pion.ICEConnectionStateClosed:
		attempt.mu.Lock()
		if attempt.result.State == attemptStatePending {
			code := "attempt-closed"
			attempt.result.State = attemptStateFailed
			attempt.result.SelectedPair = nil
			attempt.result.FailureCode = &code
		}
		attempt.mu.Unlock()
	default:
	}
}

func (attempt *pionAttempt) fail(code string) {
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.result.State != attemptStatePending {
		return
	}
	attempt.result.State = attemptStateFailed
	attempt.result.SelectedPair = nil
	attempt.result.FailureCode = &code
}

func (attempt *pionAttempt) onChallenge(
	channel challengeEchoer,
	message pion.DataChannelMessage,
) {
	attempt.mu.Lock()
	if attempt.result.State == attemptStateFailed {
		attempt.mu.Unlock()
		return
	}
	if attempt.challengeReceived {
		code := "data-channel-challenge-replayed"
		attempt.result.State = attemptStateFailed
		attempt.result.SelectedPair = nil
		attempt.result.FailureCode = &code
		attempt.mu.Unlock()
		return
	}
	if attempt.result.State != attemptStatePending {
		attempt.mu.Unlock()
		return
	}
	// Reserving the single proof frame before parsing closes the race where two
	// concurrent callbacks could both echo and ambiguously satisfy the attempt.
	attempt.challengeReceived = true
	attempt.mu.Unlock()
	challenge, err := parseDataChannelChallenge(message)
	if err != nil || challenge.ProtocolVersion != ProtocolVersion ||
		challenge.AttemptAuthority != attempt.authority ||
		subtle.ConstantTimeCompare([]byte(challenge.AttemptAuthority.Challenge), []byte(attempt.authority.Challenge)) != 1 {
		attempt.fail("data-channel-challenge-invalid")
		return
	}
	// The exact framed echo lets the browser independently prove application
	// traffic traversed the same attempt whose selected pair is reported.
	if err := channel.SendText(string(message.Data)); err != nil {
		attempt.fail("data-channel-echo-failed")
		return
	}
	attempt.mu.Lock()
	if attempt.result.State == attemptStatePending {
		attempt.applicationProven = true
		attempt.establishIfProvenLocked()
	}
	attempt.mu.Unlock()
	emitTrace(attempt.trace, TraceEvent{
		Milestone: traceChallengeObserved, InstanceID: attempt.instanceID,
		RunID:             attempt.authority.RequestAuthority.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: attempt.authority.RequestAuthority.FixtureBinding.AttestationSHA256,
		AttemptID:         attempt.authority.AttemptID, Outcome: "echoed",
	})
}

func (attempt *pionAttempt) establishIfProvenLocked() {
	if attempt.result.SelectedPair != nil && attempt.applicationProven {
		attempt.result.State = attemptStateEstablished
		attempt.result.FailureCode = nil
	}
}

func parseDataChannelChallenge(message pion.DataChannelMessage) (DataChannelChallenge, error) {
	if !message.IsString || len(message.Data) == 0 || len(message.Data) > 4096 {
		return DataChannelChallenge{}, errors.New("data-channel challenge frame is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Data))
	decoder.DisallowUnknownFields()
	var challenge DataChannelChallenge
	if err := decoder.Decode(&challenge); err != nil {
		return DataChannelChallenge{}, errors.New("data-channel challenge frame is invalid")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return DataChannelChallenge{}, errors.New("data-channel challenge frame has trailing data")
	}
	canonical, err := json.Marshal(challenge)
	if err != nil || !bytes.Equal(canonical, message.Data) {
		return DataChannelChallenge{}, errors.New("data-channel challenge frame is not canonical")
	}
	return challenge, nil
}

func challengeBindingSHA256(authority AttemptAuthority) string {
	encoded, _ := CanonicalAttemptAuthorityDocument(authority)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func selectedPair(peer *pion.PeerConnection) (SelectedPairEvidence, error) {
	if peer == nil || peer.SCTP() == nil || peer.SCTP().Transport() == nil || peer.SCTP().Transport().ICETransport() == nil {
		return SelectedPairEvidence{}, errors.New("pion ICE transport is unavailable")
	}
	pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return SelectedPairEvidence{}, errors.New("pion selected candidate pair is unavailable")
	}
	return SelectedPairEvidence{Local: candidateEvidence(pair.Local), Remote: candidateEvidence(pair.Remote)}, nil
}

func candidateEvidence(candidate *pion.ICECandidate) CandidateEvidence {
	family := "unknown"
	if address, err := netip.ParseAddr(candidate.Address); err == nil {
		switch {
		case address.Unmap().Is4():
			family = "ipv4"
		case address.Is6():
			family = "ipv6"
		}
	}
	return CandidateEvidence{
		CandidateType: candidate.Typ.String(),
		Protocol:      candidate.Protocol.String(),
		Address:       candidate.Address,
		Port:          candidate.Port,
		AddressFamily: family,
	}
}
