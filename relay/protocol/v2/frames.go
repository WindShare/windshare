package v2

import (
	"crypto/ed25519"
	"encoding/binary"
	"time"
)

const (
	SessionRetiredMagic = "WS2F"

	RegisterInitBytes             = 4 + 1 + 1 + 2 + ShareIDBytes + ShareInstanceBytes + PKHashBytes + DigestBytes + DigestBytes
	ChallengeBytes                = 4 + 1 + 1 + 2 + ChallengeIDBytes + ChallengeNonceBytes + 8
	RegisterProofBytes            = 4 + 1 + 1 + 2 + SenderPublicKeyBytes + SignatureBytes
	RegisteredBytes               = 4 + 1 + 3 + ShareIDBytes + ShareInstanceBytes + DigestBytes
	ResumeCredentialBytes         = 4 + 1 + 3 + ResumeTokenBytes
	StopInitBytes                 = 4 + 1 + 3 + ShareIDBytes + ShareInstanceBytes + PKHashBytes + RelayIdentityBytes + StopIDBytes
	StopProofBytes                = 4 + 1 + 3 + SenderPublicKeyBytes + SignatureBytes
	StoppedBytes                  = 4 + 1 + 3 + StopIDBytes
	JoinBytes                     = 4 + 1 + 3 + ShareIDBytes
	SessionRetiredBytes           = 4 + 1 + 3 + RelaySessionIDBytes
	DescriptorUploadHeaderBytes   = 4 + 1 + 3 + 4
	DescriptorDeliveryHeaderBytes = 4 + 1 + 3 + RelaySessionIDBytes + 4
	ErrorBytes                    = 4 + 1 + 1 + 2 + 4
	OpaqueRouteHeaderBytes        = 4 + 1 + 3 + RelaySessionIDBytes + 4
)

type RegisterInit struct {
	Mode             RegistrationMode
	ShareID          ShareID
	ShareInstance    ShareInstance
	PKHash           PKHash
	DescriptorDigest Digest
	ResumeTokenHash  Digest
}

func (f RegisterInit) Validate() error {
	if !f.Mode.valid() {
		return ErrMode
	}
	if !nonzero(f.ShareInstance[:]) || !validRouteIdentity(f.ShareID, f.PKHash) {
		return ErrIdentity
	}
	return nil
}

func (f RegisterInit) MarshalBinary() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	result := appendControlPrefix(nil, "WS2R", byte(f.Mode))
	result = append(result, f.ShareID[:]...)
	result = append(result, f.ShareInstance[:]...)
	result = append(result, f.PKHash[:]...)
	result = append(result, f.DescriptorDigest[:]...)
	return append(result, f.ResumeTokenHash[:]...), nil
}

func ParseRegisterInit(encoded []byte) (RegisterInit, error) {
	if len(encoded) != RegisterInitBytes || !controlPrefix(encoded, "WS2R") {
		return RegisterInit{}, ErrMalformed
	}
	frame := RegisterInit{Mode: RegistrationMode(encoded[5])}
	offset := 8
	copy(frame.ShareID[:], encoded[offset:offset+ShareIDBytes])
	offset += ShareIDBytes
	copy(frame.ShareInstance[:], encoded[offset:offset+ShareInstanceBytes])
	offset += ShareInstanceBytes
	copy(frame.PKHash[:], encoded[offset:offset+PKHashBytes])
	offset += PKHashBytes
	copy(frame.DescriptorDigest[:], encoded[offset:offset+DigestBytes])
	offset += DigestBytes
	copy(frame.ResumeTokenHash[:], encoded[offset:])
	if err := frame.Validate(); err != nil {
		return RegisterInit{}, err
	}
	return frame, nil
}

type Challenge struct {
	Purpose              ChallengePurpose
	ID                   ChallengeID
	Nonce                ChallengeNonce
	ExpiresAtUnixSeconds uint64
}

func (f Challenge) Validate() error {
	if !f.Purpose.valid() {
		return ErrPurpose
	}
	if !nonzero(f.ID[:]) || !nonzero(f.Nonce[:]) || f.ExpiresAtUnixSeconds == 0 {
		return ErrIdentity
	}
	return nil
}

func (f Challenge) MarshalBinary() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	result := appendControlPrefix(nil, "WS2Q", byte(f.Purpose))
	result = append(result, f.ID[:]...)
	result = append(result, f.Nonce[:]...)
	return binary.BigEndian.AppendUint64(result, f.ExpiresAtUnixSeconds), nil
}

func ParseChallenge(encoded []byte) (Challenge, error) {
	if len(encoded) != ChallengeBytes || !controlPrefix(encoded, "WS2Q") {
		return Challenge{}, ErrMalformed
	}
	frame := Challenge{Purpose: ChallengePurpose(encoded[5])}
	copy(frame.ID[:], encoded[8:8+ChallengeIDBytes])
	copy(frame.Nonce[:], encoded[8+ChallengeIDBytes:8+ChallengeIDBytes+ChallengeNonceBytes])
	frame.ExpiresAtUnixSeconds = binary.BigEndian.Uint64(encoded[len(encoded)-8:])
	if err := frame.Validate(); err != nil {
		return Challenge{}, err
	}
	return frame, nil
}

type RegisterProof struct {
	Mode            RegistrationMode
	SenderPublicKey [SenderPublicKeyBytes]byte
	Signature       [SignatureBytes]byte
}

func (f RegisterProof) MarshalBinary() ([]byte, error) {
	if !f.Mode.valid() || !nonzero(f.SenderPublicKey[:]) || !nonzero(f.Signature[:]) {
		return nil, ErrMalformed
	}
	result := appendControlPrefix(nil, "WS2P", byte(f.Mode))
	result = append(result, f.SenderPublicKey[:]...)
	return append(result, f.Signature[:]...), nil
}

func ParseRegisterProof(encoded []byte) (RegisterProof, error) {
	if len(encoded) != RegisterProofBytes || !controlPrefix(encoded, "WS2P") {
		return RegisterProof{}, ErrMalformed
	}
	frame := RegisterProof{Mode: RegistrationMode(encoded[5])}
	copy(frame.SenderPublicKey[:], encoded[8:8+SenderPublicKeyBytes])
	copy(frame.Signature[:], encoded[8+SenderPublicKeyBytes:])
	if !frame.Mode.valid() || !nonzero(frame.SenderPublicKey[:]) || !nonzero(frame.Signature[:]) {
		return RegisterProof{}, ErrMalformed
	}
	return frame, nil
}

type Registered struct {
	ShareID          ShareID
	ShareInstance    ShareInstance
	DescriptorDigest Digest
}

func (f Registered) MarshalBinary() ([]byte, error) {
	if !nonzero(f.ShareID[:]) || !nonzero(f.ShareInstance[:]) {
		return nil, ErrIdentity
	}
	result := appendReservedPrefix(nil, "WS2K")
	result = append(result, f.ShareID[:]...)
	result = append(result, f.ShareInstance[:]...)
	return append(result, f.DescriptorDigest[:]...), nil
}

func ParseRegistered(encoded []byte) (Registered, error) {
	if len(encoded) != RegisteredBytes || !reservedPrefix(encoded, "WS2K") {
		return Registered{}, ErrMalformed
	}
	var frame Registered
	offset := 8
	copy(frame.ShareID[:], encoded[offset:offset+ShareIDBytes])
	offset += ShareIDBytes
	copy(frame.ShareInstance[:], encoded[offset:offset+ShareInstanceBytes])
	offset += ShareInstanceBytes
	copy(frame.DescriptorDigest[:], encoded[offset:])
	if !nonzero(frame.ShareID[:]) || !nonzero(frame.ShareInstance[:]) {
		return Registered{}, ErrIdentity
	}
	return frame, nil
}

type ResumeCredential struct{ Token ResumeToken }

func (f ResumeCredential) MarshalBinary() ([]byte, error) {
	if !nonzero(f.Token[:]) {
		return nil, ErrIdentity
	}
	return append(appendReservedPrefix(nil, "WS2T"), f.Token[:]...), nil
}

func ParseResumeCredential(encoded []byte) (ResumeCredential, error) {
	if len(encoded) != ResumeCredentialBytes || !reservedPrefix(encoded, "WS2T") {
		return ResumeCredential{}, ErrMalformed
	}
	var frame ResumeCredential
	copy(frame.Token[:], encoded[8:])
	if !nonzero(frame.Token[:]) {
		return ResumeCredential{}, ErrIdentity
	}
	return frame, nil
}

type StopInit struct {
	ShareID       ShareID
	ShareInstance ShareInstance
	PKHash        PKHash
	RelayIdentity RelayIdentity
	StopID        StopID
}

func (f StopInit) Validate() error {
	if !nonzero(f.ShareInstance[:]) || !nonzero(f.RelayIdentity[:]) || !nonzero(f.StopID[:]) || !validRouteIdentity(f.ShareID, f.PKHash) {
		return ErrIdentity
	}
	return nil
}

func (f StopInit) MarshalBinary() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	result := appendReservedPrefix(nil, "WS2X")
	result = append(result, f.ShareID[:]...)
	result = append(result, f.ShareInstance[:]...)
	result = append(result, f.PKHash[:]...)
	result = append(result, f.RelayIdentity[:]...)
	return append(result, f.StopID[:]...), nil
}

func ParseStopInit(encoded []byte) (StopInit, error) {
	if len(encoded) != StopInitBytes || !reservedPrefix(encoded, "WS2X") {
		return StopInit{}, ErrMalformed
	}
	var frame StopInit
	offset := 8
	copy(frame.ShareID[:], encoded[offset:offset+ShareIDBytes])
	offset += ShareIDBytes
	copy(frame.ShareInstance[:], encoded[offset:offset+ShareInstanceBytes])
	offset += ShareInstanceBytes
	copy(frame.PKHash[:], encoded[offset:offset+PKHashBytes])
	offset += PKHashBytes
	copy(frame.RelayIdentity[:], encoded[offset:offset+RelayIdentityBytes])
	offset += RelayIdentityBytes
	copy(frame.StopID[:], encoded[offset:])
	if err := frame.Validate(); err != nil {
		return StopInit{}, err
	}
	return frame, nil
}

type StopProof struct {
	SenderPublicKey [SenderPublicKeyBytes]byte
	Signature       [SignatureBytes]byte
}

func (f StopProof) MarshalBinary() ([]byte, error) {
	if !nonzero(f.SenderPublicKey[:]) || !nonzero(f.Signature[:]) {
		return nil, ErrMalformed
	}
	result := appendReservedPrefix(nil, "WS2V")
	result = append(result, f.SenderPublicKey[:]...)
	return append(result, f.Signature[:]...), nil
}

func ParseStopProof(encoded []byte) (StopProof, error) {
	if len(encoded) != StopProofBytes || !reservedPrefix(encoded, "WS2V") {
		return StopProof{}, ErrMalformed
	}
	var frame StopProof
	copy(frame.SenderPublicKey[:], encoded[8:8+SenderPublicKeyBytes])
	copy(frame.Signature[:], encoded[8+SenderPublicKeyBytes:])
	if !nonzero(frame.SenderPublicKey[:]) || !nonzero(frame.Signature[:]) {
		return StopProof{}, ErrMalformed
	}
	return frame, nil
}

type Stopped struct{ StopID StopID }

func (f Stopped) MarshalBinary() ([]byte, error) {
	if !nonzero(f.StopID[:]) {
		return nil, ErrIdentity
	}
	return append(appendReservedPrefix(nil, "WS2Y"), f.StopID[:]...), nil
}

func ParseStopped(encoded []byte) (Stopped, error) {
	if len(encoded) != StoppedBytes || !reservedPrefix(encoded, "WS2Y") {
		return Stopped{}, ErrMalformed
	}
	var frame Stopped
	copy(frame.StopID[:], encoded[8:])
	if !nonzero(frame.StopID[:]) {
		return Stopped{}, ErrIdentity
	}
	return frame, nil
}

type Join struct{ ShareID ShareID }

func (f Join) MarshalBinary() ([]byte, error) {
	if !nonzero(f.ShareID[:]) {
		return nil, ErrIdentity
	}
	return append(appendReservedPrefix(nil, "WS2J"), f.ShareID[:]...), nil
}

func ParseJoin(encoded []byte) (Join, error) {
	if len(encoded) != JoinBytes || !reservedPrefix(encoded, "WS2J") {
		return Join{}, ErrMalformed
	}
	var frame Join
	copy(frame.ShareID[:], encoded[8:])
	if !nonzero(frame.ShareID[:]) {
		return Join{}, ErrIdentity
	}
	return frame, nil
}

// SessionRetired is an allocation-free relay lifecycle signal. It carries no
// E2E authority; clients apply it only to an already materialized exact channel.
type SessionRetired struct{ RelaySessionID RelaySessionID }

func (f SessionRetired) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	return append(appendReservedPrefix(nil, SessionRetiredMagic), f.RelaySessionID[:]...), nil
}

func ParseSessionRetired(encoded []byte) (SessionRetired, error) {
	if len(encoded) != SessionRetiredBytes || !reservedPrefix(encoded, SessionRetiredMagic) {
		return SessionRetired{}, ErrMalformed
	}
	var frame SessionRetired
	copy(frame.RelaySessionID[:], encoded[8:])
	if !nonzero(frame.RelaySessionID[:]) {
		return SessionRetired{}, ErrIdentity
	}
	return frame, nil
}

func appendControlPrefix(destination []byte, magic string, discriminator byte) []byte {
	destination = append(destination, magic...)
	return append(destination, WireVersion, discriminator, 0, 0)
}

func appendReservedPrefix(destination []byte, magic string) []byte {
	destination = append(destination, magic...)
	return append(destination, WireVersion, 0, 0, 0)
}

func controlPrefix(encoded []byte, magic string) bool {
	return len(encoded) >= 8 && string(encoded[:4]) == magic && encoded[4] == WireVersion && encoded[6] == 0 && encoded[7] == 0
}

func reservedPrefix(encoded []byte, magic string) bool {
	return len(encoded) >= 8 && string(encoded[:4]) == magic && encoded[4] == WireVersion && encoded[5] == 0 && encoded[6] == 0 && encoded[7] == 0
}

func publicKey(raw [SenderPublicKeyBytes]byte) ed25519.PublicKey { return cloneBytes(raw[:]) }

type ErrorCode uint16

const (
	ErrorMalformed ErrorCode = iota + 1
	ErrorUnsupportedMode
	ErrorShareIDCollision
	ErrorAlreadyRegistered
	ErrorChallengeExpired
	ErrorInvalidProof
	ErrorDescriptorInvalid
	ErrorNotFound
	ErrorStarting
	ErrorAdmission
	ErrorStopped
)

func (c ErrorCode) valid() bool { return c >= ErrorMalformed && c <= ErrorStopped }

type ErrorFrame struct {
	Code       ErrorCode
	RetryAfter time.Duration
}

func (f ErrorFrame) MarshalBinary() ([]byte, error) {
	if !f.Code.valid() || f.RetryAfter < 0 || f.RetryAfter > 30*time.Second || f.RetryAfter%time.Millisecond != 0 ||
		(f.Code != ErrorStarting && f.Code != ErrorAdmission && f.RetryAfter != 0) {
		return nil, ErrMalformed
	}
	result := append([]byte("WS2E"), WireVersion, 0)
	result = binary.BigEndian.AppendUint16(result, uint16(f.Code))
	return binary.BigEndian.AppendUint32(result, uint32(f.RetryAfter/time.Millisecond)), nil
}

func ParseError(encoded []byte) (ErrorFrame, error) {
	if len(encoded) != ErrorBytes || string(encoded[:4]) != "WS2E" || encoded[4] != WireVersion || encoded[5] != 0 {
		return ErrorFrame{}, ErrMalformed
	}
	frame := ErrorFrame{
		Code:       ErrorCode(binary.BigEndian.Uint16(encoded[6:8])),
		RetryAfter: time.Duration(binary.BigEndian.Uint32(encoded[8:])) * time.Millisecond,
	}
	if _, err := frame.MarshalBinary(); err != nil {
		return ErrorFrame{}, err
	}
	return frame, nil
}
