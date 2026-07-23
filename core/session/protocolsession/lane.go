package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
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

	LaneGrantTTL            = 30 * time.Second
	LaneGrantTombstoneTTL   = 30 * time.Second
	DefaultMaxLogicalLanes  = 16
	DefaultMaxPendingGrants = 64
	MaxLaneRetryAfter       = 30 * time.Second
)

const (
	laneHelloMagic   = "WS2A"
	laneAcceptMagic  = "WS2B"
	laneRejectMagic  = "WS2N"
	laneHelloDomain  = "windshare/v2 lane-hello\x00"
	laneAcceptDomain = "windshare/v2 lane-accept\x00"
	laneRejectDomain = "windshare/v2 lane-reject\x00"
)

var (
	ErrLaneInput       = errors.New("protocol session lane input is invalid")
	ErrLaneMalformed   = errors.New("protocol session lane frame is malformed")
	ErrLaneProof       = errors.New("protocol session lane proof is invalid")
	ErrLaneSignature   = errors.New("protocol session lane signature is invalid")
	ErrLaneGrantBudget = errors.New("protocol session lane grant budget is exhausted")
	ErrLaneEpoch       = errors.New("protocol session lane epoch is exhausted")
	ErrLaneUnknown     = errors.New("protocol session logical lane is unknown")
	ErrLaneStopping    = errors.New("protocol session is stopping")
)

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

func ParseLaneAccept(encoded []byte, hello LaneHello, senderPublicKey ed25519.PublicKey) ([]byte, error) {
	if len(encoded) != LaneAcceptBytes || !hello.valid || len(senderPublicKey) != ed25519.PublicKeySize || string(encoded[:4]) != laneAcceptMagic || encoded[4] != WireVersion {
		return nil, ErrLaneMalformed
	}
	helloDigest := sha256.Sum256(hello.encoded[:])
	if !bytes.Equal(encoded[5:5+sha256.Size], helloDigest[:]) {
		return nil, ErrLaneSignature
	}
	body := encoded[:LaneAcceptBodyBytes]
	digest := sha256.Sum256(body)
	if !ed25519.Verify(senderPublicKey, append([]byte(laneAcceptDomain), digest[:]...), encoded[LaneAcceptBodyBytes:]) {
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

func ParseLaneReject(encoded []byte, hello LaneHello, senderPublicKey ed25519.PublicKey) (LaneRejection, error) {
	if len(encoded) != LaneRejectBytes || !hello.valid || len(senderPublicKey) != ed25519.PublicKeySize || string(encoded[:4]) != laneRejectMagic || encoded[4] != WireVersion || encoded[6] != 0 || encoded[7] != 0 {
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
	if !ed25519.Verify(senderPublicKey, append([]byte(laneRejectDomain), digest[:]...), encoded[LaneRejectBodyBytes:]) {
		return LaneRejection{}, ErrLaneSignature
	}
	return LaneRejection{Code: code, RetryAfter: retry}, nil
}

type LaneGrant struct {
	LaneID      uint32
	LaneEpoch   uint32
	OperationID OperationID
	AttachNonce [LaneAttachNonceBytes]byte
	ExpiresAt   time.Time
}

type laneGrantState struct {
	grant      LaneGrant
	consumedAt time.Time
}

type logicalLaneState struct {
	lastEpoch uint32
	active    bool
}

type LaneRegistryConfig struct {
	ShareInstance     catalog.ShareInstance
	ProtocolSessionID ProtocolSessionID
	ReceiverToSender  TrafficKey
	SenderSigningKey  ed25519.PrivateKey
	InitialLaneID     uint32
	MaxLogicalLanes   int
	MaxPendingGrants  int
	Now               func() time.Time
}

type LaneRegistry struct {
	mu               sync.Mutex
	share            catalog.ShareInstance
	session          ProtocolSessionID
	receiverToSender TrafficKey
	senderSigningKey ed25519.PrivateKey
	now              func() time.Time
	maxLogical       int
	maxPending       int
	nextEpoch        uint32
	nextLaneID       uint32
	activeCount      int
	stopping         bool
	lanes            map[uint32]logicalLaneState
	grants           map[OperationID]laneGrantState
}

func NewLaneRegistry(config LaneRegistryConfig) (*LaneRegistry, error) {
	if config.ShareInstance.IsZero() || config.ProtocolSessionID.IsZero() || !config.ReceiverToSender.valid || config.ReceiverToSender.direction != DirectionReceiverToSender ||
		len(config.SenderSigningKey) != ed25519.PrivateKeySize || config.InitialLaneID == 0 {
		return nil, ErrLaneInput
	}
	if config.MaxLogicalLanes == 0 {
		config.MaxLogicalLanes = DefaultMaxLogicalLanes
	}
	if config.MaxPendingGrants == 0 {
		config.MaxPendingGrants = DefaultMaxPendingGrants
	}
	if config.MaxLogicalLanes < 1 || config.MaxPendingGrants < 1 {
		return nil, ErrLaneInput
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LaneRegistry{
		share: config.ShareInstance, session: config.ProtocolSessionID, receiverToSender: config.ReceiverToSender,
		senderSigningKey: bytes.Clone(config.SenderSigningKey), now: config.Now, maxLogical: config.MaxLogicalLanes,
		maxPending: config.MaxPendingGrants, nextLaneID: 1, activeCount: 1,
		lanes: map[uint32]logicalLaneState{config.InitialLaneID: {active: true}}, grants: make(map[OperationID]laneGrantState),
	}, nil
}

func (r *LaneRegistry) IssueGrant(requestedLaneID uint32, operationID OperationID, attachNonce []byte) (LaneGrant, error) {
	if r == nil || operationID.IsZero() || len(attachNonce) != LaneAttachNonceBytes || !laneNonzero(attachNonce) {
		return LaneGrant{}, ErrLaneInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return LaneGrant{}, ErrLaneStopping
	}
	now := r.now()
	r.cleanup(now)
	if len(r.grants) >= r.maxPending {
		return LaneGrant{}, ErrLaneGrantBudget
	}
	if _, exists := r.grants[operationID]; exists {
		return LaneGrant{}, ErrLaneInput
	}
	laneID := requestedLaneID
	if r.nextEpoch == ^uint32(0) {
		return LaneGrant{}, ErrLaneEpoch
	}
	if laneID == 0 {
		if len(r.lanes) >= r.maxLogical {
			return LaneGrant{}, ErrLaneGrantBudget
		}
		var err error
		laneID, err = r.allocateLaneID()
		if err != nil {
			return LaneGrant{}, err
		}
		r.lanes[laneID] = logicalLaneState{}
	} else if _, exists := r.lanes[laneID]; !exists {
		return LaneGrant{}, ErrLaneUnknown
	}
	r.nextEpoch++
	grant := LaneGrant{LaneID: laneID, LaneEpoch: r.nextEpoch, OperationID: operationID, ExpiresAt: now.Add(LaneGrantTTL)}
	copy(grant.AttachNonce[:], attachNonce)
	r.grants[operationID] = laneGrantState{grant: grant}
	return grant, nil
}

// RevokeGrant removes only the exact grant returned by IssueGrant. LaneEpoch and
// nonce comparison prevent a delayed sender operation from revoking same-ID work.
func (r *LaneRegistry) RevokeGrant(grant LaneGrant) bool {
	if r == nil || grant.OperationID.IsZero() || grant.LaneID == 0 || grant.LaneEpoch == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.grants[grant.OperationID]
	if !exists || state.grant != grant || !state.consumedAt.IsZero() {
		return false
	}
	delete(r.grants, grant.OperationID)
	r.reclaimUnclaimedLaneLocked(grant.LaneID)
	return true
}

func (r *LaneRegistry) reclaimUnclaimedLaneLocked(laneID uint32) {
	lane, exists := r.lanes[laneID]
	if !exists || lane.active || lane.lastEpoch != 0 {
		return
	}
	for _, state := range r.grants {
		if state.grant.LaneID == laneID {
			return
		}
	}
	delete(r.lanes, laneID)
}

func (r *LaneRegistry) allocateLaneID() (uint32, error) {
	for range uint64(^uint32(0)) {
		candidate := r.nextLaneID
		r.nextLaneID++
		if r.nextLaneID == 0 {
			r.nextLaneID = 1
		}
		if candidate != 0 {
			if _, exists := r.lanes[candidate]; !exists {
				return candidate, nil
			}
		}
	}
	return 0, ErrLaneGrantBudget
}

type LaneAdmissionDisposition uint8

const (
	LaneAdmissionAccepted LaneAdmissionDisposition = iota + 1
	LaneAdmissionRejected
	LaneAdmissionSilentClose
)

type LaneAdmission struct {
	Disposition LaneAdmissionDisposition
	LaneID      uint32
	LaneEpoch   uint32
	Rejection   LaneRejectCode
	Response    []byte
}

// AdmitCandidate returns SilentClose for malformed or unauthenticated input.
// A signed rejection is legal only after the traffic-key proof authenticates the peer.
func (r *LaneRegistry) AdmitCandidate(encoded, senderNonce []byte) (LaneAdmission, error) {
	if r == nil {
		return LaneAdmission{Disposition: LaneAdmissionSilentClose}, ErrLaneInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	hello, err := ParseLaneHello(encoded, r.receiverToSender)
	if err != nil {
		return LaneAdmission{Disposition: LaneAdmissionSilentClose}, err
	}
	now := r.now()
	r.cleanup(now)
	reject := func(code LaneRejectCode, retry time.Duration) (LaneAdmission, error) {
		response, buildErr := NewLaneReject(hello, LaneRejection{Code: code, RetryAfter: retry}, r.senderSigningKey)
		return LaneAdmission{Disposition: LaneAdmissionRejected, LaneID: hello.laneID, LaneEpoch: hello.laneEpoch, Rejection: code, Response: response}, buildErr
	}
	if hello.share != r.share || hello.session != r.session {
		return reject(LaneRejectUnknownSession, 0)
	}
	state, exists := r.grants[hello.operationID]
	if !exists {
		return reject(LaneRejectGrantConsumed, 0)
	}
	grant := state.grant
	if grant.LaneID != hello.laneID || grant.LaneEpoch != hello.laneEpoch || !bytes.Equal(grant.AttachNonce[:], hello.attachNonce[:]) {
		return reject(LaneRejectGrantMismatch, 0)
	}
	if r.stopping {
		return reject(LaneRejectStopping, 0)
	}
	if !state.consumedAt.IsZero() {
		return reject(LaneRejectGrantConsumed, 0)
	}
	if !now.Before(grant.ExpiresAt) {
		return reject(LaneRejectGrantExpired, 0)
	}
	lane := r.lanes[grant.LaneID]
	if grant.LaneEpoch <= lane.lastEpoch {
		return reject(LaneRejectStaleEpoch, 0)
	}
	if lane.active || r.activeCount >= r.maxLogical {
		return reject(LaneRejectAdmissionLimited, time.Second)
	}
	response, err := NewLaneAccept(hello, senderNonce, r.senderSigningKey)
	if err != nil {
		return LaneAdmission{Disposition: LaneAdmissionSilentClose}, err
	}
	state.consumedAt = now
	r.grants[hello.operationID] = state
	lane.lastEpoch, lane.active = grant.LaneEpoch, true
	r.lanes[grant.LaneID] = lane
	r.activeCount++
	return LaneAdmission{Disposition: LaneAdmissionAccepted, LaneID: grant.LaneID, LaneEpoch: grant.LaneEpoch, Response: response}, nil
}

func (r *LaneRegistry) Release(laneID, laneEpoch uint32) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lane, exists := r.lanes[laneID]
	if !exists || !lane.active || lane.lastEpoch != laneEpoch {
		return false
	}
	lane.active = false
	r.lanes[laneID] = lane
	r.activeCount--
	return true
}

func (r *LaneRegistry) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
}

func (r *LaneRegistry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopping = true
	clear(r.grants)
	clear(r.lanes)
	r.activeCount = 0
	clear(r.senderSigningKey)
	r.senderSigningKey = nil
	r.receiverToSender.Destroy()
	r.mu.Unlock()
}

func (r *LaneRegistry) cleanup(now time.Time) {
	for operationID, state := range r.grants {
		retainedUntil := state.grant.ExpiresAt.Add(LaneGrantTombstoneTTL)
		if !state.consumedAt.IsZero() {
			retainedUntil = state.consumedAt.Add(LaneGrantTombstoneTTL)
		}
		if !now.Before(retainedUntil) {
			delete(r.grants, operationID)
			r.reclaimUnclaimedLaneLocked(state.grant.LaneID)
		}
	}
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
