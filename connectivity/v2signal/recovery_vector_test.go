package v2signal

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type peerFailureVector struct {
	Code  uint16                                   `json:"code"`
	Scope protocolsession.PeerFailureRecoveryScope `json:"scope"`
}

func buildPeerFailureVectors() []peerFailureVector {
	var result []peerFailureVector
	for code := uint16(0x5000); code <= 0x500c; code++ {
		result = append(result, peerFailureVector{code, protocolsession.PeerFailureScope(code)})
	}
	return append(result, peerFailureVector{0x5fff, protocolsession.PeerFailurePathTerminal})
}

type peerPathControlVector struct {
	Kind          protocolsession.PeerPathControlKind `json:"kind"`
	BodyB64       string                              `json:"bodyB64"`
	SignedBodyB64 string                              `json:"signedBodyB64"`
	Sequence      string                              `json:"sequence"`
}

func buildPeerPathControlVectors(t *testing.T, key ed25519.PrivateKey, base protocolsession.ControlBinding) []peerPathControlVector {
	t.Helper()
	var result []peerPathControlVector
	for kind := protocolsession.PeerPathDemand; kind <= protocolsession.PeerPathNetworkChanged; kind++ {
		control := protocolsession.PeerPathControl{PeerPathID: [16]byte(testBinding().PeerPathID), NetworkGenerationID: [16]byte{9}, ControlSequence: uint64(kind), Kind: kind, ValidFor: time.Minute, ProviderProfile: "pion-4.2.16-ice-4.2.7-windows"}
		if kind == protocolsession.PeerPathRevoke {
			control.ValidFor = 0
		}
		if kind == protocolsession.PeerPathDemand {
			control.HoldFor = 40 * time.Second
		}
		body, err := protocolsession.EncodePeerPathControl(control)
		if err != nil {
			t.Fatal(err)
		}
		binding := base
		binding.MessageKind = protocolsession.MessagePeerPathControl
		binding.Sequence = 20 + uint64(kind)
		signed, err := protocolsession.SignControlBody(key, protocolsession.ControlDomainPeerPath, binding, body)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, peerPathControlVector{kind, base64.StdEncoding.EncodeToString(body), base64.StdEncoding.EncodeToString(signed), strconv.FormatUint(binding.Sequence, 10)})
	}
	return result
}

type peerBoundErrorVector struct {
	Code          uint16 `json:"code"`
	BodyB64       string `json:"bodyB64"`
	SignedBodyB64 string `json:"signedBodyB64"`
	Sequence      string `json:"sequence"`
}

func buildPeerBoundErrorVectors(t *testing.T, key ed25519.PrivateKey, base protocolsession.ControlBinding, operationID protocolsession.OperationID) []peerBoundErrorVector {
	t.Helper()
	binding := protocolBinding(testBinding())
	var result []peerBoundErrorVector
	for index, code := range []uint16{protocolsession.PeerOperationCodeTimeout, protocolsession.PeerOperationCodeAuthentication, 0x5fff} {
		body, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{Scope: protocolsession.OperationScopePeer, Code: code, Message: "Peer attempt failed", PeerAttempt: &binding})
		if err != nil {
			t.Fatal(err)
		}
		base.MessageKind = protocolsession.MessageOperationError
		base.OperationID = operationID
		base.HasOperationID = true
		base.Sequence = uint64(40 + index)
		signed, err := protocolsession.SignControlBody(key, protocolsession.ControlDomainOperation, base, body)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, peerBoundErrorVector{code, base64.StdEncoding.EncodeToString(body), base64.StdEncoding.EncodeToString(signed), strconv.FormatUint(base.Sequence, 10)})
	}
	return result
}
