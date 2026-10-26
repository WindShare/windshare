package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

const (
	LaneGrantTTL            = 30 * time.Second
	LaneGrantTombstoneTTL   = 30 * time.Second
	DefaultMaxLogicalLanes  = 16
	DefaultMaxPendingGrants = 64
)

var (
	ErrLaneGrantBudget         = errors.New("protocol session lane grant budget is exhausted")
	ErrLaneEpoch               = errors.New("protocol session lane epoch is exhausted")
	ErrLaneUnknown             = errors.New("protocol session logical lane is unknown")
	ErrLaneStopping            = errors.New("protocol session is stopping")
	ErrLaneSettlementUnclaimed = errors.New("protocol session authenticated lane settlement was not claimed")
)

type laneGrantState struct {
	grant      LaneGrant
	expiresAt  time.Time
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
	closing          bool
	settlementCount  int
	settlementDone   chan struct{}
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
	if r.nextEpoch == ^uint32(0) {
		return LaneGrant{}, ErrLaneEpoch
	}
	laneID, err := r.grantLaneID(requestedLaneID, now)
	if err != nil {
		return LaneGrant{}, err
	}
	r.nextEpoch++
	grant := LaneGrant{LaneID: laneID, LaneEpoch: r.nextEpoch, OperationID: operationID}
	copy(grant.AttachNonce[:], attachNonce)
	r.grants[operationID] = laneGrantState{grant: grant, expiresAt: now.Add(LaneGrantTTL)}
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

func (r *LaneRegistry) grantLaneID(requested uint32, now time.Time) (uint32, error) {
	if requested != 0 {
		if _, exists := r.lanes[requested]; !exists {
			return 0, ErrLaneUnknown
		}
		return requested, nil
	}
	if available := r.availableLaneID(now); available != 0 {
		return available, nil
	}
	if len(r.lanes) >= r.maxLogical {
		return 0, ErrLaneGrantBudget
	}
	laneID, err := r.allocateLaneID()
	if err != nil {
		return 0, err
	}
	r.lanes[laneID] = logicalLaneState{}
	return laneID, nil
}

// Automatic grants reserve a session slot, not a lifetime-new identity. Keep
// lastEpoch when recycling so delayed proofs and releases cannot affect its new
// occupant. Outstanding usable grants reserve their slots until expiry/revocation.
func (r *LaneRegistry) availableLaneID(now time.Time) uint32 {
	var available uint32
	for laneID, lane := range r.lanes {
		if lane.active || available != 0 && laneID >= available {
			continue
		}
		reserved := false
		for _, state := range r.grants {
			if state.grant.LaneID == laneID && state.consumedAt.IsZero() &&
				state.grant.LaneEpoch > lane.lastEpoch && now.Before(state.expiresAt) {
				reserved = true
				break
			}
		}
		if !reserved {
			available = laneID
		}
	}
	return available
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
	Disposition     LaneAdmissionDisposition
	SettlementBegan bool
	OperationID     OperationID
	LaneID          uint32
	LaneEpoch       uint32
	Rejection       LaneRejection
	Response        []byte
}

// LaneAdmissionSettlementGate transfers terminal authority to the exact
// connectivity attempt after WS2A authentication and before registry state or
// signed evidence changes. Implementations must make this a one-shot state
// transition; false leaves the grant untouched and produces no response.
type LaneAdmissionSettlementGate interface {
	BeginAuthenticatedSettlement(OperationID, LaneAdmissionIdentity) bool
}

type LaneAdmissionIdentity struct {
	LaneID    uint32
	LaneEpoch uint32
}

type LaneAdmissionSettlementGateFunc func(OperationID, LaneAdmissionIdentity) bool

func (function LaneAdmissionSettlementGateFunc) BeginAuthenticatedSettlement(
	operationID OperationID,
	lane LaneAdmissionIdentity,
) bool {
	return function != nil && function(operationID, lane)
}

// AdmitCandidate returns SilentClose for malformed, unauthenticated, or
// unclaimed input. A signed response or registry mutation is legal only after
// the traffic-key proof and the caller's synchronous settlement transition.
func (r *LaneRegistry) AdmitCandidate(
	encoded,
	senderNonce []byte,
	settlement LaneAdmissionSettlementGate,
) (LaneAdmission, error) {
	if r == nil || settlement == nil {
		return LaneAdmission{Disposition: LaneAdmissionSilentClose}, ErrLaneInput
	}
	// Authentication uses a registry-owned traffic key, so it remains serialized
	// with Close while the caller-controlled gate stays outside the registry lock.
	r.mu.Lock()
	hello, err := ParseLaneHello(encoded, r.receiverToSender)
	if err != nil {
		r.mu.Unlock()
		return LaneAdmission{Disposition: LaneAdmissionSilentClose}, err
	}
	identity := LaneAdmission{
		Disposition: LaneAdmissionSilentClose,
		OperationID: hello.operationID,
		LaneID:      hello.laneID,
		LaneEpoch:   hello.laneEpoch,
	}
	if r.closing {
		r.mu.Unlock()
		return identity, ErrLaneStopping
	}
	r.beginSettlementLocked()
	r.mu.Unlock()
	if !r.claimAuthenticatedSettlement(settlement, hello.operationID, LaneAdmissionIdentity{
		LaneID: hello.laneID, LaneEpoch: hello.laneEpoch,
	}) {
		return identity, ErrLaneSettlementUnclaimed
	}
	identity.SettlementBegan = true

	r.mu.Lock()
	defer func() {
		r.finishSettlementLocked()
		r.mu.Unlock()
	}()
	now := r.now()
	r.cleanup(now)
	reject := func(code LaneRejectCode, retry time.Duration) (LaneAdmission, error) {
		rejection := LaneRejection{Code: code, RetryAfter: retry}
		response, buildErr := NewLaneReject(hello, rejection, r.senderSigningKey)
		result := identity
		result.Disposition = LaneAdmissionRejected
		result.Rejection = rejection
		result.Response = response
		return result, buildErr
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
	if !now.Before(state.expiresAt) {
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
		return identity, err
	}
	state.consumedAt = now
	r.grants[hello.operationID] = state
	lane.lastEpoch, lane.active = grant.LaneEpoch, true
	r.lanes[grant.LaneID] = lane
	r.activeCount++
	identity.Disposition = LaneAdmissionAccepted
	identity.Response = response
	return identity, nil
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
	r.closing = true
	settlementDone := r.settlementDone
	r.mu.Unlock()
	if settlementDone != nil {
		<-settlementDone
	}
	r.mu.Lock()
	clear(r.grants)
	clear(r.lanes)
	r.activeCount = 0
	clear(r.senderSigningKey)
	r.senderSigningKey = nil
	r.receiverToSender.Destroy()
	r.mu.Unlock()
}

func (r *LaneRegistry) beginSettlementLocked() {
	if r.settlementCount == 0 {
		r.settlementDone = make(chan struct{})
	}
	r.settlementCount++
}

func (r *LaneRegistry) claimAuthenticatedSettlement(
	gate LaneAdmissionSettlementGate,
	operationID OperationID,
	lane LaneAdmissionIdentity,
) (claimed bool) {
	// A faulty control gate must not strand registry shutdown. Panics still
	// propagate because this is lifecycle authority, but its in-flight lease is
	// released first.
	defer func() {
		if !claimed {
			r.mu.Lock()
			r.finishSettlementLocked()
			r.mu.Unlock()
		}
	}()
	return gate.BeginAuthenticatedSettlement(operationID, lane)
}

func (r *LaneRegistry) finishSettlementLocked() {
	r.settlementCount--
	if r.settlementCount == 0 {
		close(r.settlementDone)
		r.settlementDone = nil
	}
}

func (r *LaneRegistry) cleanup(now time.Time) {
	for operationID, state := range r.grants {
		retainedUntil := state.expiresAt.Add(LaneGrantTombstoneTTL)
		if !state.consumedAt.IsZero() {
			retainedUntil = state.consumedAt.Add(LaneGrantTombstoneTTL)
		}
		if !now.Before(retainedUntil) {
			delete(r.grants, operationID)
			r.reclaimUnclaimedLaneLocked(state.grant.LaneID)
		}
	}
}
