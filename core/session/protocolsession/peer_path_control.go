package protocolsession

import (
	"bytes"
	"errors"
	"time"

	"github.com/fxamacker/cbor/v2"
)

const (
	MaxPeerPathControlLifetime  = 120 * time.Second
	MaxPeerPathControlBodyBytes = 256
	peerPathControlSchema       = uint64(2)
	peerPathControlFields       = 8
	MaxPeerProviderProfileBytes = 64
)

type PeerPathControlKind uint8

const (
	PeerPathDemand PeerPathControlKind = iota + 1
	PeerPathRevoke
	PeerPathMappingReady
	PeerPathNetworkChanged
)

// PeerPathControl has session authority, never negotiation-operation authority.
// Durations are relative to authenticated receipt, avoiding remote clock trust.
type PeerPathControl struct {
	PeerPathID          [IdentityBytes]byte
	NetworkGenerationID [IdentityBytes]byte
	ControlSequence     uint64
	Kind                PeerPathControlKind
	ValidFor            time.Duration
	HoldFor             time.Duration
	// ProviderProfile is an opaque local implementation evidence identity.
	// Connectivity interprets it; the protocol never grants a transport capability.
	ProviderProfile string
}

var ErrPeerPathControl = errors.New("invalid peer path control")

func validPeerProviderProfile(profile string) bool {
	if len(profile) > MaxPeerProviderProfileBytes {
		return false
	}
	for _, c := range profile {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func EncodePeerPathControl(control PeerPathControl) ([]byte, error) {
	if !validPeerProviderProfile(control.ProviderProfile) {
		return nil, ErrPeerPathControl
	}
	if control.PeerPathID == [IdentityBytes]byte{} || control.NetworkGenerationID == [IdentityBytes]byte{} ||
		control.ControlSequence == 0 || control.Kind < PeerPathDemand || control.Kind > PeerPathNetworkChanged {
		return nil, ErrPeerPathControl
	}
	if control.ValidFor < 0 || control.ValidFor > MaxPeerPathControlLifetime || control.ValidFor%time.Millisecond != 0 ||
		control.HoldFor < 0 || control.HoldFor > control.ValidFor || control.HoldFor%time.Millisecond != 0 {
		return nil, ErrPeerPathControl
	}
	if (control.Kind == PeerPathRevoke) != (control.ValidFor == 0) ||
		(control.Kind != PeerPathDemand && control.HoldFor != 0) {
		return nil, ErrPeerPathControl
	}
	return EncodeBody([]any{
		peerPathControlSchema, control.PeerPathID[:], control.NetworkGenerationID[:], control.ControlSequence,
		uint64(control.Kind), uint64(control.ValidFor / time.Millisecond), uint64(control.HoldFor / time.Millisecond), control.ProviderProfile,
	})
}

func DecodePeerPathControl(body []byte) (PeerPathControl, error) {
	var fields []cbor.RawMessage
	var control PeerPathControl
	if len(body) > MaxPeerPathControlBodyBytes {
		return control, ErrPeerPathControl
	}
	if err := messageDecMode.Unmarshal(body, &fields); err != nil || len(fields) != peerPathControlFields {
		return control, ErrPeerPathControl
	}
	var schema, kind, validFor, holdFor uint64
	var path, generation []byte
	for _, item := range []struct {
		body   []byte
		target any
	}{
		{fields[0], &schema}, {fields[1], &path}, {fields[2], &generation}, {fields[3], &control.ControlSequence},
		{fields[4], &kind}, {fields[5], &validFor}, {fields[6], &holdFor}, {fields[7], &control.ProviderProfile},
	} {
		if err := messageDecMode.Unmarshal(item.body, item.target); err != nil {
			return PeerPathControl{}, ErrPeerPathControl
		}
	}
	if schema != peerPathControlSchema || len(path) != IdentityBytes || len(generation) != IdentityBytes ||
		kind > uint64(PeerPathNetworkChanged) || validFor > uint64(MaxPeerPathControlLifetime/time.Millisecond) || holdFor > validFor {
		return PeerPathControl{}, ErrPeerPathControl
	}
	copy(control.PeerPathID[:], path)
	copy(control.NetworkGenerationID[:], generation)
	control.Kind = PeerPathControlKind(kind)
	control.ValidFor = time.Duration(validFor) * time.Millisecond
	control.HoldFor = time.Duration(holdFor) * time.Millisecond
	canonical, err := EncodePeerPathControl(control)
	if err != nil || !bytes.Equal(canonical, body) {
		return PeerPathControl{}, ErrPeerPathControl
	}
	return control, nil
}
