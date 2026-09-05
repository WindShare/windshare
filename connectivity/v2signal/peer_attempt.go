package v2signal

import "github.com/windshare/windshare/core/session/protocolsession"

func (authority peerContinuationAuthority) PeerAttemptBinding() protocolsession.PeerAttemptBinding {
	return protocolBinding(authority.binding)
}

func protocolBinding(binding Binding) protocolsession.PeerAttemptBinding {
	return protocolsession.PeerAttemptBinding{
		PeerPathID: [16]byte(binding.PeerPathID), AttemptID: [16]byte(binding.AttemptID), AttemptSequence: binding.AttemptSequence,
	}
}

func (classifier OperationContinuationClassifier) ClassifyPeerAttemptContinuation(kind protocolsession.MessageKind, body []byte) (protocolsession.PeerAttemptBinding, bool, error) {
	switch kind {
	case protocolsession.MessagePeerOffer:
		offer, err := DecodeOffer(body)
		return protocolBinding(offer.Binding), true, err
	case protocolsession.MessagePeerAnswer:
		answer, err := DecodeAnswer(body)
		return protocolBinding(answer.Binding), true, err
	case protocolsession.MessagePeerCandidate:
		candidate, err := DecodeCandidate(body)
		return protocolBinding(candidate.Binding), true, err
	default:
		return protocolsession.PeerAttemptBinding{}, false, nil
	}
}

func (validator ReceiverControlValidator) ClassifyPeerAttemptContinuation(kind protocolsession.MessageKind, body []byte) (protocolsession.PeerAttemptBinding, bool, error) {
	return OperationContinuationClassifier(validator).ClassifyPeerAttemptContinuation(kind, body)
}
