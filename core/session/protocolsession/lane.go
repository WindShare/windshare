package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/senderauth"
)

const (
	LaneAttachNonceBytes = 16
	LaneSenderNonceBytes = 16
	LaneHelloBodyBytes   = 4 + 1 + catalog.IdentityBytes + IdentityBytes + 4 + 4 + IdentityBytes + LaneAttachNonceBytes
	LaneHelloBytes       = LaneHelloBodyBytes + sha256.Size
	LaneAcceptBodyBytes  = 4 + 1 + sha256.Size + LaneSenderNonceBytes
	LaneAcceptBytes      = LaneAcceptBodyBytes + ed25519.SignatureSize
	LaneRejectBodyBytes  = 4 + 1 + 1 + 2 + sha256.Size + 4
	LaneRejectBytes      = LaneRejectBodyBytes + ed25519.SignatureSize

	MaxLaneRetryAfter = 30 * time.Second
)

const (
	laneHelloMagic   = "WS2A"
	laneAcceptMagic  = "WS2B"
	laneRejectMagic  = "WS2N"
	laneHelloDomain  = "windshare/v2 lane-hello\x00"
	laneAcceptDomain = "windshare/v2 lane-accept\x00"
	laneRejectDomain = "windshare/v2 lane-reject\x00"
)

const (
	laneAttachBodyVersion     = uint64(1)
	laneAttachRequestMode     = uint64(0)
	laneAttachGrantMode       = uint64(1)
	laneAttachRequestFields   = 3
	laneAttachGrantFields     = 5
	laneAttachMaxNestedLevels = 4
	laneAttachMaxMapPairs     = 16
)

var laneAttachDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:       cbor.DupMapKeyEnforcedAPF,
		IndefLength:     cbor.IndefLengthForbidden,
		TagsMd:          cbor.TagsForbidden,
		MaxNestedLevels: laneAttachMaxNestedLevels,
		MaxMapPairs:     laneAttachMaxMapPairs,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var (
	ErrLaneInput     = errors.New("protocol session lane input is invalid")
	ErrLaneMalformed = errors.New("protocol session lane frame is malformed")
	ErrLaneProof     = errors.New("protocol session lane proof is invalid")
	ErrLaneSignature = errors.New("protocol session lane signature is invalid")
)

type LaneAttachRequest struct {
	RequestedLaneID uint32
}

// LaneGrant carries the protocol authorization for one channel attachment.
// OperationID is bound by the enclosing message; expiry remains private to the
// sender's registry because it is not conveyed to the receiver.
type LaneGrant struct {
	LaneID      uint32
	LaneEpoch   uint32
	OperationID OperationID
	AttachNonce [LaneAttachNonceBytes]byte
}

func EncodeLaneAttachRequest(requestedLaneID uint32) ([]byte, error) {
	return EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachRequestMode,
		2: uint64(requestedLaneID),
	})
}

func DecodeLaneAttachRequest(encoded []byte) (LaneAttachRequest, error) {
	fields, err := decodeLaneAttachBody(encoded, laneAttachRequestFields)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachRequestMode {
		return LaneAttachRequest{}, errors.Join(ErrLaneMalformed, err)
	}
	requested, ok := fields[2].(uint64)
	if !ok || requested > uint64(^uint32(0)) {
		return LaneAttachRequest{}, ErrLaneMalformed
	}
	return LaneAttachRequest{RequestedLaneID: uint32(requested)}, nil
}

func EncodeLaneGrant(grant LaneGrant) ([]byte, error) {
	if grant.OperationID.IsZero() {
		return nil, ErrInvalidOperationID
	}
	if grant.LaneID == 0 || grant.LaneEpoch == 0 || !laneNonzero(grant.AttachNonce[:]) {
		return nil, ErrLaneInput
	}
	return EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachGrantMode,
		2: uint64(grant.LaneID),
		3: uint64(grant.LaneEpoch),
		4: grant.AttachNonce[:],
	})
}

func DecodeLaneGrant(encoded []byte, operationID OperationID) (LaneGrant, error) {
	if operationID.IsZero() {
		return LaneGrant{}, ErrInvalidOperationID
	}
	fields, err := decodeLaneAttachBody(encoded, laneAttachGrantFields)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachGrantMode {
		return LaneGrant{}, errors.Join(ErrLaneMalformed, err)
	}
	laneID, laneOK := fields[2].(uint64)
	epoch, epochOK := fields[3].(uint64)
	nonce, nonceOK := fields[4].([]byte)
	if !laneOK || !epochOK || !nonceOK || laneID == 0 || laneID > uint64(^uint32(0)) ||
		epoch == 0 || epoch > uint64(^uint32(0)) || len(nonce) != LaneAttachNonceBytes || !laneNonzero(nonce) {
		return LaneGrant{}, ErrLaneMalformed
	}
	grant := LaneGrant{LaneID: uint32(laneID), LaneEpoch: uint32(epoch), OperationID: operationID}
	copy(grant.AttachNonce[:], nonce)
	return grant, nil
}

func decodeLaneAttachBody(encoded []byte, fieldCount int) (map[uint64]any, error) {
	var fields map[uint64]any
	if err := laneAttachDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != fieldCount {
		return nil, errors.Join(ErrLaneMalformed, err)
	}
	canonical, err := EncodeBody(fields)
	if err != nil {
		return nil, errors.Join(ErrLaneMalformed, err)
	}
	if !bytes.Equal(canonical, encoded) {
		return nil, errors.Join(ErrLaneMalformed, ErrNonCanonicalMessage)
	}
	return fields, nil
}

type LaneHello struct {
	encoded     [LaneHelloBytes]byte
	share       catalog.ShareInstance
	session     ProtocolSessionID
	laneID      uint32
	laneEpoch   uint32
	operationID OperationID
	attachNonce [LaneAttachNonceBytes]byte
	valid       bool
}

func NewLaneHello(share catalog.ShareInstance, session ProtocolSessionID, laneID, laneEpoch uint32, operationID OperationID, attachNonce []byte, receiverToSender TrafficKey) (LaneHello, error) {
	if share.IsZero() || session.IsZero() || laneID == 0 || laneEpoch == 0 || operationID.IsZero() || len(attachNonce) != LaneAttachNonceBytes ||
		!laneNonzero(attachNonce) || !receiverToSender.valid || receiverToSender.direction != DirectionReceiverToSender {
		return LaneHello{}, ErrLaneInput
	}
	body := make([]byte, 0, LaneHelloBodyBytes)
	body = append(body, laneHelloMagic...)
	body = append(body, WireVersion)
	body = append(body, share.Bytes()...)
	body = append(body, session[:]...)
	body = binary.BigEndian.AppendUint32(body, laneID)
	body = binary.BigEndian.AppendUint32(body, laneEpoch)
	body = append(body, operationID[:]...)
	body = append(body, attachNonce...)
	proof := laneProof(receiverToSender.value[:], body)
	return laneHelloFromBody(body, proof)
}

func ParseLaneHello(encoded []byte, receiverToSender TrafficKey) (LaneHello, error) {
	if len(encoded) != LaneHelloBytes || !receiverToSender.valid || receiverToSender.direction != DirectionReceiverToSender {
		return LaneHello{}, ErrLaneMalformed
	}
	body := encoded[:LaneHelloBodyBytes]
	if string(body[:4]) != laneHelloMagic || body[4] != WireVersion {
		return LaneHello{}, ErrLaneMalformed
	}
	want := laneProof(receiverToSender.value[:], body)
	if !hmac.Equal(want, encoded[LaneHelloBodyBytes:]) {
		return LaneHello{}, ErrLaneProof
	}
	return laneHelloFromBody(body, encoded[LaneHelloBodyBytes:])
}

// UntrustedLaneHelloRoute extracts only the identities needed to select the
// candidate ProtocolSession. Callers must not authorize or answer from this
// result: LaneRegistry.AdmitCandidate performs the traffic-key proof before any
// signed response or resource admission is legal.
func UntrustedLaneHelloRoute(encoded []byte) (catalog.ShareInstance, ProtocolSessionID, error) {
	if len(encoded) != LaneHelloBytes || string(encoded[:4]) != laneHelloMagic || encoded[4] != WireVersion {
		return catalog.ShareInstance{}, ProtocolSessionID{}, ErrLaneMalformed
	}
	share, err := catalog.ShareInstanceFromBytes(encoded[5 : 5+catalog.IdentityBytes])
	if err != nil || share.IsZero() {
		return catalog.ShareInstance{}, ProtocolSessionID{}, ErrLaneMalformed
	}
	sessionOffset := 5 + catalog.IdentityBytes
	session, err := ProtocolSessionIDFromBytes(encoded[sessionOffset : sessionOffset+IdentityBytes])
	if err != nil || session.IsZero() {
		return catalog.ShareInstance{}, ProtocolSessionID{}, ErrLaneMalformed
	}
	return share, session, nil
}

func laneHelloFromBody(body, proof []byte) (LaneHello, error) {
	share, err := catalog.ShareInstanceFromBytes(body[5 : 5+catalog.IdentityBytes])
	if err != nil || share.IsZero() {
		return LaneHello{}, ErrLaneMalformed
	}
	sessionOffset := 5 + catalog.IdentityBytes
	session, err := ProtocolSessionIDFromBytes(body[sessionOffset : sessionOffset+IdentityBytes])
	if err != nil || session.IsZero() {
		return LaneHello{}, ErrLaneMalformed
	}
	laneOffset := sessionOffset + IdentityBytes
	laneID := binary.BigEndian.Uint32(body[laneOffset : laneOffset+4])
	laneEpoch := binary.BigEndian.Uint32(body[laneOffset+4 : laneOffset+8])
	operationOffset := laneOffset + 8
	operationID, err := OperationIDFromBytes(body[operationOffset : operationOffset+IdentityBytes])
	if err != nil || laneID == 0 || laneEpoch == 0 || operationID.IsZero() {
		return LaneHello{}, ErrLaneMalformed
	}
	var hello LaneHello
	copy(hello.encoded[:LaneHelloBodyBytes], body)
	copy(hello.encoded[LaneHelloBodyBytes:], proof)
	hello.share, hello.session, hello.laneID, hello.laneEpoch, hello.operationID = share, session, laneID, laneEpoch, operationID
	copy(hello.attachNonce[:], body[operationOffset+IdentityBytes:])
	hello.valid = true
	return hello, nil
}

func (h LaneHello) Encoded() []byte                      { return bytes.Clone(h.encoded[:]) }
func (h LaneHello) ShareInstance() catalog.ShareInstance { return h.share }
func (h LaneHello) ProtocolSessionID() ProtocolSessionID { return h.session }
func (h LaneHello) LaneID() uint32                       { return h.laneID }
func (h LaneHello) LaneEpoch() uint32                    { return h.laneEpoch }
func (h LaneHello) OperationID() OperationID             { return h.operationID }
func (h LaneHello) AttachNonce() []byte                  { return bytes.Clone(h.attachNonce[:]) }

func NewLaneAccept(hello LaneHello, senderNonce []byte, senderPrivateKey ed25519.PrivateKey) ([]byte, error) {
	if !hello.valid || len(senderNonce) != LaneSenderNonceBytes || !laneNonzero(senderNonce) || len(senderPrivateKey) != ed25519.PrivateKeySize {
		return nil, ErrLaneInput
	}
	helloDigest := sha256.Sum256(hello.encoded[:])
	body := make([]byte, 0, LaneAcceptBodyBytes)
	body = append(body, laneAcceptMagic...)
	body = append(body, WireVersion)
	body = append(body, helloDigest[:]...)
	body = append(body, senderNonce...)
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(senderPrivateKey, append([]byte(laneAcceptDomain), digest[:]...))
	return append(body, signature...), nil
}

func ParseLaneAccept(encoded []byte, hello LaneHello, sender *senderauth.Verifier) ([]byte, error) {
	if len(encoded) != LaneAcceptBytes || !hello.valid || !sender.Valid() || string(encoded[:4]) != laneAcceptMagic || encoded[4] != WireVersion {
		return nil, ErrLaneMalformed
	}
	helloDigest := sha256.Sum256(hello.encoded[:])
	if !bytes.Equal(encoded[5:5+sha256.Size], helloDigest[:]) {
		return nil, ErrLaneSignature
	}
	body := encoded[:LaneAcceptBodyBytes]
	digest := sha256.Sum256(body)
	if !sender.Verify(append([]byte(laneAcceptDomain), digest[:]...), encoded[LaneAcceptBodyBytes:]) {
		return nil, ErrLaneSignature
	}
	return bytes.Clone(body[5+sha256.Size:]), nil
}

type LaneRejectCode uint8

const (
	LaneRejectUnknownSession LaneRejectCode = iota + 1
	LaneRejectStaleEpoch
	LaneRejectGrantExpired
	LaneRejectGrantConsumed
	LaneRejectAdmissionLimited
	LaneRejectStopping
	LaneRejectGrantMismatch
)

func (c LaneRejectCode) valid() bool {
	return c >= LaneRejectUnknownSession && c <= LaneRejectGrantMismatch
}

type LaneRejection struct {
	Code       LaneRejectCode
	RetryAfter time.Duration
}

func NewLaneReject(hello LaneHello, rejection LaneRejection, senderPrivateKey ed25519.PrivateKey) ([]byte, error) {
	if !hello.valid || !rejection.Code.valid() || len(senderPrivateKey) != ed25519.PrivateKeySize || rejection.RetryAfter < 0 || rejection.RetryAfter > MaxLaneRetryAfter ||
		(rejection.Code != LaneRejectAdmissionLimited && rejection.RetryAfter != 0) || rejection.RetryAfter%time.Millisecond != 0 {
		return nil, ErrLaneInput
	}
	helloDigest := sha256.Sum256(hello.encoded[:])
	body := make([]byte, 0, LaneRejectBodyBytes)
	body = append(body, laneRejectMagic...)
	body = append(body, WireVersion, byte(rejection.Code), 0, 0)
	body = append(body, helloDigest[:]...)
	body = binary.BigEndian.AppendUint32(body, uint32(rejection.RetryAfter/time.Millisecond))
	digest := sha256.Sum256(body)
	return append(body, ed25519.Sign(senderPrivateKey, append([]byte(laneRejectDomain), digest[:]...))...), nil
}

func ParseLaneReject(encoded []byte, hello LaneHello, sender *senderauth.Verifier) (LaneRejection, error) {
	if len(encoded) != LaneRejectBytes || !hello.valid || !sender.Valid() || string(encoded[:4]) != laneRejectMagic || encoded[4] != WireVersion || encoded[6] != 0 || encoded[7] != 0 {
		return LaneRejection{}, ErrLaneMalformed
	}
	code := LaneRejectCode(encoded[5])
	if !code.valid() {
		return LaneRejection{}, ErrLaneMalformed
	}
	helloDigest := sha256.Sum256(hello.encoded[:])
	if !bytes.Equal(encoded[8:8+sha256.Size], helloDigest[:]) {
		return LaneRejection{}, ErrLaneSignature
	}
	retry := time.Duration(binary.BigEndian.Uint32(encoded[8+sha256.Size:LaneRejectBodyBytes])) * time.Millisecond
	if retry > MaxLaneRetryAfter || (code != LaneRejectAdmissionLimited && retry != 0) {
		return LaneRejection{}, ErrLaneMalformed
	}
	body := encoded[:LaneRejectBodyBytes]
	digest := sha256.Sum256(body)
	if !sender.Verify(append([]byte(laneRejectDomain), digest[:]...), encoded[LaneRejectBodyBytes:]) {
		return LaneRejection{}, ErrLaneSignature
	}
	return LaneRejection{Code: code, RetryAfter: retry}, nil
}

func laneProof(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(laneHelloDomain))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

func laneNonzero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value LaneRejectCode) DiagnosticName() string {
	switch value {
	case LaneRejectUnknownSession:
		return "unknown-session"
	case LaneRejectStaleEpoch:
		return "stale-epoch"
	case LaneRejectGrantConsumed:
		return "grant-consumed"
	case LaneRejectGrantExpired:
		return "grant-expired"
	case LaneRejectAdmissionLimited:
		return "admission-limited"
	case LaneRejectStopping:
		return "stopping"
	case LaneRejectGrantMismatch:
		return "grant-mismatch"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}
