// Package v2signal owns the E2E WebRTC signaling schema. Provider, TURN,
// relay-node, and path-cost policy deliberately remain outside core/session.
package v2signal

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/text/unicode/norm"
)

const (
	IdentityBytes          = 16
	SignalingSchemaVersion = 1
	MaximumSDPBytes        = 60 << 10
	MaximumCandidateBytes  = 4 << 10
	MaximumSDPMidBytes     = 256
	MaximumUsernameBytes   = 256
)

// MessageKind mirrors the frozen E2E operation registry without importing the
// independent core module. Root release builds must remain valid with GOWORK
// disabled; the shared vector is the cross-module authority for these values.
type MessageKind uint8

const (
	MessagePeerOffer     MessageKind = 16
	MessagePeerAnswer    MessageKind = 17
	MessagePeerCandidate MessageKind = 18
)

var (
	ErrInvalidSignal      = errors.New("v2 peer signaling is invalid")
	ErrNonCanonicalSignal = errors.New("v2 peer signaling is not canonical")
	ErrSignalBinding      = errors.New("v2 peer signaling changed path or attempt identity")
)

type PeerPathID [IdentityBytes]byte
type AttemptID [IdentityBytes]byte

type Binding struct {
	PeerPathID PeerPathID
	AttemptID  AttemptID
}

func (binding Binding) Validate() error {
	if zero(binding.PeerPathID[:]) || zero(binding.AttemptID[:]) {
		return ErrInvalidSignal
	}
	return nil
}

// RequireSame enforces that the first offer reserves the sole path/attempt
// identity for every answer and candidate in this R-track negotiation.
func (binding Binding) RequireSame(candidate Binding) error {
	if binding.Validate() != nil || candidate.Validate() != nil || binding != candidate {
		return ErrSignalBinding
	}
	return nil
}

type Offer struct {
	Binding Binding
	SDP     string
}

func (Offer) Kind() MessageKind { return MessagePeerOffer }

type Answer struct {
	Binding Binding
	SDP     string
}

func (Answer) Kind() MessageKind { return MessagePeerAnswer }

type Candidate struct {
	Binding          Binding
	Candidate        string
	SDPMid           *string
	SDPMLineIndex    *uint16
	UsernameFragment *string
}

func (Candidate) Kind() MessageKind { return MessagePeerCandidate }

var signalEncMode = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var signalDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden,
		TagsMd: cbor.TagsForbidden, MaxNestedLevels: 4, MaxArrayElements: 16, MaxMapPairs: 16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func EncodeOffer(offer Offer) ([]byte, error) {
	if err := validateDescription(offer.Binding, offer.SDP); err != nil {
		return nil, err
	}
	return encodeDescription(offer.Binding, offer.SDP)
}

func DecodeOffer(encoded []byte) (Offer, error) {
	binding, sdp, err := decodeDescription(encoded)
	if err != nil {
		return Offer{}, err
	}
	offer := Offer{Binding: binding, SDP: sdp}
	canonical, _ := EncodeOffer(offer)
	if !bytes.Equal(canonical, encoded) {
		return Offer{}, ErrNonCanonicalSignal
	}
	return offer, nil
}

func EncodeAnswer(answer Answer) ([]byte, error) {
	if err := validateDescription(answer.Binding, answer.SDP); err != nil {
		return nil, err
	}
	return encodeDescription(answer.Binding, answer.SDP)
}

func DecodeAnswer(encoded []byte) (Answer, error) {
	binding, sdp, err := decodeDescription(encoded)
	if err != nil {
		return Answer{}, err
	}
	answer := Answer{Binding: binding, SDP: sdp}
	canonical, _ := EncodeAnswer(answer)
	if !bytes.Equal(canonical, encoded) {
		return Answer{}, ErrNonCanonicalSignal
	}
	return answer, nil
}

func EncodeCandidate(candidate Candidate) ([]byte, error) {
	if err := validateCandidate(candidate); err != nil {
		return nil, err
	}
	return signalEncMode.Marshal([]any{
		uint64(SignalingSchemaVersion), candidate.Binding.PeerPathID[:], candidate.Binding.AttemptID[:],
		candidate.Candidate, candidate.SDPMid, candidate.SDPMLineIndex, candidate.UsernameFragment,
	})
}

func DecodeCandidate(encoded []byte) (Candidate, error) {
	var fields []cbor.RawMessage
	if err := signalDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 7 {
		return Candidate{}, ErrInvalidSignal
	}
	binding, err := decodePrefix(fields[:3])
	if err != nil {
		return Candidate{}, err
	}
	var candidate Candidate
	candidate.Binding = binding
	if err := signalDecMode.Unmarshal(fields[3], &candidate.Candidate); err != nil {
		return Candidate{}, ErrInvalidSignal
	}
	if candidate.SDPMid, err = decodeOptionalString(fields[4]); err != nil {
		return Candidate{}, err
	}
	if candidate.SDPMLineIndex, err = decodeOptionalUint16(fields[5]); err != nil {
		return Candidate{}, err
	}
	if candidate.UsernameFragment, err = decodeOptionalString(fields[6]); err != nil {
		return Candidate{}, err
	}
	if err := validateCandidate(candidate); err != nil {
		return Candidate{}, err
	}
	canonical, _ := EncodeCandidate(candidate)
	if !bytes.Equal(canonical, encoded) {
		return Candidate{}, ErrNonCanonicalSignal
	}
	return candidate, nil
}

func encodeDescription(binding Binding, sdp string) ([]byte, error) {
	return signalEncMode.Marshal([]any{
		uint64(SignalingSchemaVersion), binding.PeerPathID[:], binding.AttemptID[:], sdp,
	})
}

func decodeDescription(encoded []byte) (Binding, string, error) {
	var fields []cbor.RawMessage
	if err := signalDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 4 {
		return Binding{}, "", ErrInvalidSignal
	}
	binding, err := decodePrefix(fields[:3])
	if err != nil {
		return Binding{}, "", err
	}
	var sdp string
	if err := signalDecMode.Unmarshal(fields[3], &sdp); err != nil || validateDescription(binding, sdp) != nil {
		return Binding{}, "", ErrInvalidSignal
	}
	return binding, sdp, nil
}

func decodePrefix(fields []cbor.RawMessage) (Binding, error) {
	if len(fields) != 3 {
		return Binding{}, ErrInvalidSignal
	}
	var version uint64
	if err := signalDecMode.Unmarshal(fields[0], &version); err != nil || version != SignalingSchemaVersion {
		return Binding{}, ErrInvalidSignal
	}
	path, err := decodeIdentity[PeerPathID](fields[1])
	if err != nil {
		return Binding{}, err
	}
	attempt, err := decodeIdentity[AttemptID](fields[2])
	if err != nil {
		return Binding{}, err
	}
	binding := Binding{PeerPathID: path, AttemptID: attempt}
	return binding, binding.Validate()
}

func decodeIdentity[T ~[IdentityBytes]byte](encoded []byte) (T, error) {
	var raw []byte
	if err := signalDecMode.Unmarshal(encoded, &raw); err != nil || len(raw) != IdentityBytes || zero(raw) {
		return T{}, ErrInvalidSignal
	}
	var result T
	copy(result[:], raw)
	return result, nil
}

func decodeOptionalString(encoded []byte) (*string, error) {
	if bytes.Equal(encoded, []byte{0xf6}) {
		return nil, nil
	}
	var value string
	if err := signalDecMode.Unmarshal(encoded, &value); err != nil {
		return nil, ErrInvalidSignal
	}
	return &value, nil
}

func decodeOptionalUint16(encoded []byte) (*uint16, error) {
	if bytes.Equal(encoded, []byte{0xf6}) {
		return nil, nil
	}
	var value uint64
	if err := signalDecMode.Unmarshal(encoded, &value); err != nil || value > uint64(^uint16(0)) {
		return nil, ErrInvalidSignal
	}
	result := uint16(value)
	return &result, nil
}

func validateDescription(binding Binding, sdp string) error {
	if binding.Validate() != nil || !validText(sdp, MaximumSDPBytes, false) {
		return ErrInvalidSignal
	}
	return nil
}

func validateCandidate(candidate Candidate) error {
	if candidate.Binding.Validate() != nil || !validText(candidate.Candidate, MaximumCandidateBytes, false) ||
		!validOptionalText(candidate.SDPMid, MaximumSDPMidBytes) ||
		!validOptionalText(candidate.UsernameFragment, MaximumUsernameBytes) {
		return ErrInvalidSignal
	}
	return nil
}

func validOptionalText(value *string, maximum int) bool {
	return value == nil || validText(*value, maximum, true)
}

func validText(value string, maximum int, allowEmpty bool) bool {
	return (allowEmpty || value != "") && len(value) <= maximum && utf8.ValidString(value) && norm.NFC.IsNormalString(value)
}

func zero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func (binding Binding) String() string {
	return fmt.Sprintf("peer-path=%x attempt=%x", binding.PeerPathID, binding.AttemptID)
}
