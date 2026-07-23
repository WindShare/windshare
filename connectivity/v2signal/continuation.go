package v2signal

import (
	"crypto/sha256"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MaximumCandidates        = 64
	DefaultMaximumCandidates = MaximumCandidates
)

var ErrContinuationLimit = errors.New("v2 peer continuation limit is invalid")

const peerContinuationScopeDomain = "windshare/v2 peer-continuation-scope\x00"

// OperationContinuationClassifier is the transport-schema half of core's
// generation authority. It decodes only canonical suite-02 signaling values;
// replay ownership and lifecycle remain in protocolsession.
type OperationContinuationClassifier struct {
	MaximumCandidates int
}

func (classifier OperationContinuationClassifier) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	canonicalBody []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	if _, err := classifier.maximumCandidates(); err != nil {
		return protocolsession.OperationContinuationScope{}, true, err
	}
	candidate, err := DecodeCandidate(canonicalBody)
	if err != nil {
		return protocolsession.OperationContinuationScope{}, true, err
	}
	return peerContinuationScope(candidate.Binding), true, nil
}

func (classifier OperationContinuationClassifier) BeginOperationContinuation(
	requestKind protocolsession.MessageKind,
	canonicalRequestBody []byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	if requestKind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	offer, err := DecodeOffer(canonicalRequestBody)
	if err != nil {
		return nil, true, err
	}
	maximum, err := classifier.maximumCandidates()
	if err != nil {
		return nil, true, err
	}
	return peerContinuationAuthority{binding: offer.Binding, maximum: maximum}, true, nil
}

func (classifier OperationContinuationClassifier) maximumCandidates() (int, error) {
	maximum := classifier.MaximumCandidates
	if maximum == 0 {
		maximum = DefaultMaximumCandidates
	}
	if maximum < 1 || maximum > MaximumCandidates {
		return 0, ErrContinuationLimit
	}
	return maximum, nil
}

type peerContinuationAuthority struct {
	binding Binding
	maximum int
}

func (authority peerContinuationAuthority) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	canonicalBody []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	candidate, err := DecodeCandidate(canonicalBody)
	if err != nil {
		return [sha256.Size]byte{}, true, err
	}
	if err := authority.binding.RequireSame(candidate.Binding); err != nil {
		return [sha256.Size]byte{}, true, err
	}
	canonical, err := EncodeCandidate(candidate)
	if err != nil {
		return [sha256.Size]byte{}, true, err
	}
	return sha256.Sum256(canonical), true, nil
}

func (authority peerContinuationAuthority) MaximumContinuations() int {
	return authority.maximum
}

func (authority peerContinuationAuthority) OperationContinuationScope() protocolsession.OperationContinuationScope {
	return peerContinuationScope(authority.binding)
}

func peerContinuationScope(binding Binding) protocolsession.OperationContinuationScope {
	digest := sha256.New()
	_, _ = digest.Write([]byte(peerContinuationScopeDomain))
	_, _ = digest.Write(binding.PeerPathID[:])
	_, _ = digest.Write(binding.AttemptID[:])
	var scope protocolsession.OperationContinuationScope
	copy(scope[:], digest.Sum(nil))
	return scope
}

var _ protocolsession.OperationContinuationClassifier = OperationContinuationClassifier{}
var _ protocolsession.OperationContinuationAuthority = peerContinuationAuthority{}
