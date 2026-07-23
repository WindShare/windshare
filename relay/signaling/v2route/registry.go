// Package v2route owns relay-visible v2 route and session lifecycle state.
package v2route

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	SenderCrashGrace    = 60 * time.Second
	JoinStartingGrace   = 5 * time.Second
	SessionTombstoneTTL = 60 * time.Second
	sessionIDRetries    = 4
)

var (
	ErrConfig            = errors.New("relay v2 route: invalid configuration")
	ErrAdmission         = errors.New("relay v2 route: admission budget exhausted")
	ErrCollision         = errors.New("relay v2 route: share ID collision")
	ErrAlreadyRegistered = errors.New("relay v2 route: share already registered")
	ErrStopped           = errors.New("relay v2 route: share instance is stopped")
	ErrNotFound          = errors.New("relay v2 route: share was not found")
	ErrOwner             = errors.New("relay v2 route: connection does not own route")
	ErrResume            = errors.New("relay v2 route: resume credential is invalid")
	ErrSession           = errors.New("relay v2 route: relay session is invalid")
	ErrStopping          = errors.New("relay v2 route: STOP is in progress")
	ErrCommitFailed      = errors.New("relay v2 route: STOP was not committed")
	ErrCommitUncertain   = errors.New("relay v2 route: STOP durability is uncertain")
)

type ConnectionID string

// ConnectionRef identifies one local lifetime of a wire-visible ConnectionID.
// The capability token is deliberately opaque: routing code may compare exact
// references, but it cannot reconstruct authority from an ID after locks have
// been released.
type ConnectionRef struct {
	id         ConnectionID
	generation *connectionGeneration
}

type connectionGeneration struct {
	traceID uint64
}

var nextConnectionGeneration atomic.Uint64

func NewConnectionRef(id ConnectionID) (ConnectionRef, error) {
	if id == "" {
		return ConnectionRef{}, ErrConfig
	}
	return ConnectionRef{
		id: id,
		generation: &connectionGeneration{
			traceID: nextConnectionGeneration.Add(1),
		},
	}, nil
}

func (connection ConnectionRef) ConnectionID() ConnectionID {
	return connection.id
}

// LocalGeneration is an observability label, not authority. Exact ownership is
// carried by the unexported capability token so even counter wrap cannot make a
// delayed retirement equal a replacement connection.
func (connection ConnectionRef) LocalGeneration() uint64 {
	if connection.generation == nil {
		return 0
	}
	return connection.generation.traceID
}

func (connection ConnectionRef) Valid() bool {
	return connection.id != "" && connection.generation != nil
}

type Config struct {
	MaxRoutes int
	// MaxSessions bounds active sessions plus 60-second ended-ID tombstones.
	MaxSessions         int
	MaxSessionsPerShare int
	Random              io.Reader
	Now                 func() time.Time
	Tombstones          TombstoneStore
}

type routeState uint8

const (
	routeStarting routeState = iota + 1
	routeLive
	routeGrace
	routeStopped
	routeStopUncertain
)

type route struct {
	init          v2.RegisterInit
	state         routeState
	owner         ConnectionRef
	descriptor    []byte
	startDeadline time.Time
	graceDeadline time.Time
	stopID        v2.StopID
	pendingStop   *stopTransaction
}

type relaySession struct {
	shareID  v2.ShareID
	sender   ConnectionRef
	receiver ConnectionRef
}

type sessionTombstone struct {
	session   relaySession
	expiresAt time.Time
}

type SessionDisposition uint8

const (
	SessionForward SessionDisposition = iota + 1
	SessionRetired
)

type SessionResolution struct {
	Disposition SessionDisposition
	Destination ConnectionRef
}

type SessionRetirement struct {
	RelaySessionID v2.RelaySessionID
	Sender         ConnectionRef
	Receiver       ConnectionRef
}

type RouteRetirement struct {
	Owner    ConnectionRef
	Sessions []SessionRetirement
}

type Registry struct {
	mu sync.Mutex

	maxRoutes           int
	maxSessions         int
	maxSessionsPerShare int
	random              io.Reader
	now                 func() time.Time
	tombstones          TombstoneStore
	routes              map[v2.ShareID]*route
	sessions            map[v2.RelaySessionID]relaySession
	sessionTombstones   map[v2.RelaySessionID]sessionTombstone
	sessionAuthorities  map[v2.ShareID]int
}

func New(ctx context.Context, config Config) (*Registry, error) {
	if config.MaxRoutes <= 0 || config.MaxSessions <= 0 || config.MaxSessionsPerShare <= 0 ||
		config.MaxSessionsPerShare > config.MaxSessions || config.Random == nil || config.Tombstones == nil {
		return nil, ErrConfig
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	stopped, err := config.Tombstones.Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(stopped) > config.MaxRoutes {
		return nil, ErrAdmission
	}
	registry := &Registry{
		maxRoutes: config.MaxRoutes, maxSessions: config.MaxSessions, maxSessionsPerShare: config.MaxSessionsPerShare,
		random: config.Random, now: config.Now, tombstones: config.Tombstones,
		routes: make(map[v2.ShareID]*route), sessions: make(map[v2.RelaySessionID]relaySession),
		sessionTombstones: make(map[v2.RelaySessionID]sessionTombstone), sessionAuthorities: make(map[v2.ShareID]int),
	}
	for _, tombstone := range stopped {
		if !validTombstone(tombstone) {
			return nil, ErrConfig
		}
		if _, duplicate := registry.routes[tombstone.ShareID]; duplicate {
			return nil, ErrConfig
		}
		registry.routes[tombstone.ShareID] = &route{
			init: v2.RegisterInit{
				Mode: v2.RegistrationFresh, ShareID: tombstone.ShareID,
				ShareInstance: tombstone.ShareInstance, PKHash: tombstone.PKHash,
			},
			state: routeStopped, stopID: tombstone.StopID,
		}
	}
	return registry, nil
}

// BeginRegistration reserves route capacity before descriptor upload. Abort must
// be called on any later proof or descriptor failure.
func (r *Registry) BeginRegistration(init v2.RegisterInit, owner ConnectionRef) error {
	if r == nil || init.Mode != v2.RegistrationFresh || init.Validate() != nil || !owner.Valid() {
		return ErrConfig
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireRoutes(now)
	r.expireSessionTombstones(now)
	if existing := r.routes[init.ShareID]; existing != nil {
		if (existing.state == routeStopped || existing.state == routeStopUncertain) &&
			existing.init.ShareInstance == init.ShareInstance {
			return ErrStopped
		}
		if subtle.ConstantTimeCompare(existing.init.PKHash[:], init.PKHash[:]) != 1 {
			return ErrCollision
		}
		return ErrAlreadyRegistered
	}
	if len(r.routes) >= r.maxRoutes {
		return ErrAdmission
	}
	r.routes[init.ShareID] = &route{
		init: init, state: routeStarting, owner: owner, startDeadline: now.Add(JoinStartingGrace),
	}
	return nil
}

func (r *Registry) AbortRegistration(shareID v2.ShareID, owner ConnectionRef) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.routes[shareID]
	if current == nil || current.state != routeStarting || current.owner != owner || current.pendingStop != nil {
		return false
	}
	delete(r.routes, shareID)
	return true
}

// Publish accepts only the unforgeable value returned after challenge, digest,
// size, and descriptor-signature verification. The registry owns its bytes.
func (r *Registry) Publish(shareID v2.ShareID, owner ConnectionRef, descriptor v2.VerifiedDescriptor) error {
	if r == nil {
		return ErrConfig
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.routes[shareID]
	if current == nil {
		return ErrNotFound
	}
	if current.pendingStop != nil {
		return ErrStopping
	}
	if current.state == routeStarting && !r.now().Before(current.startDeadline) {
		delete(r.routes, shareID)
		return ErrNotFound
	}
	if current.state != routeStarting || current.owner != owner {
		return ErrOwner
	}
	object, authorized := descriptor.ObjectFor(current.init)
	if !authorized {
		return ErrOwner
	}
	current.descriptor = object
	current.state = routeLive
	current.startDeadline = time.Time{}
	return nil
}

func (r *Registry) Resume(init v2.RegisterInit, authority v2.SenderAuthority, owner ConnectionRef, token v2.ResumeToken) error {
	if r == nil || init.Mode != v2.RegistrationResume || init.Validate() != nil || !authority.Authorizes(init) || !owner.Valid() {
		return ErrResume
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireRoutes(now)
	r.expireSessionTombstones(now)
	current := r.routes[init.ShareID]
	if current == nil {
		return ErrNotFound
	}
	if current.state == routeStopped || current.state == routeStopUncertain {
		return ErrStopped
	}
	if current.pendingStop != nil {
		return ErrStopping
	}
	if current.state != routeGrace {
		return ErrNotFound
	}
	if current.init.ShareInstance != init.ShareInstance || subtle.ConstantTimeCompare(current.init.PKHash[:], init.PKHash[:]) != 1 ||
		subtle.ConstantTimeCompare(current.init.DescriptorDigest[:], init.DescriptorDigest[:]) != 1 {
		return ErrResume
	}
	tokenHash := sha256.Sum256(token[:])
	if subtle.ConstantTimeCompare(tokenHash[:], current.init.ResumeTokenHash[:]) != 1 ||
		subtle.ConstantTimeCompare(init.ResumeTokenHash[:], current.init.ResumeTokenHash[:]) != 1 {
		return ErrResume
	}
	current.owner = owner
	current.state = routeLive
	current.graceDeadline = time.Time{}
	return nil
}

// ValidateResumeCredential is the pre-challenge admission boundary required by
// the wire contract. A bad token must not consume challenge capacity, while the
// later Resume call repeats every check under the same registry lock so this
// method never becomes an authorization grant on its own.
func (r *Registry) ValidateResumeCredential(init v2.RegisterInit, token v2.ResumeToken) error {
	if r == nil || init.Mode != v2.RegistrationResume || init.Validate() != nil {
		return ErrResume
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireRoutes(now)
	r.expireSessionTombstones(now)
	current := r.routes[init.ShareID]
	if current == nil {
		return ErrNotFound
	}
	if current.state == routeStopped || current.state == routeStopUncertain {
		return ErrStopped
	}
	if current.pendingStop != nil {
		return ErrStopping
	}
	if current.state != routeGrace {
		return ErrNotFound
	}
	if current.init.ShareInstance != init.ShareInstance ||
		subtle.ConstantTimeCompare(current.init.PKHash[:], init.PKHash[:]) != 1 ||
		subtle.ConstantTimeCompare(current.init.DescriptorDigest[:], init.DescriptorDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(current.init.ResumeTokenHash[:], init.ResumeTokenHash[:]) != 1 {
		return ErrResume
	}
	tokenHash := sha256.Sum256(token[:])
	if subtle.ConstantTimeCompare(tokenHash[:], current.init.ResumeTokenHash[:]) != 1 {
		return ErrResume
	}
	return nil
}

// UnexpectedDisconnect enters recoverable grace and returns the exact peers
// whose endpoint state must be retired. A concurrent durable STOP fences new
// joins, but sender teardown still retires receivers immediately rather than
// waiting behind storage latency.
func (r *Registry) UnexpectedDisconnect(shareID v2.ShareID, owner ConnectionRef) (RouteRetirement, bool) {
	if r == nil {
		return RouteRetirement{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.routes[shareID]
	if current == nil || current.owner != owner || (current.state != routeLive && current.state != routeStarting) {
		return RouteRetirement{}, false
	}
	retirement := RouteRetirement{Owner: owner, Sessions: r.dropShareSessions(shareID)}
	if current.pendingStop != nil {
		current.pendingStop.ownerDisconnected = true
		current.owner = ConnectionRef{}
		return retirement, true
	}
	if current.state == routeStarting {
		delete(r.routes, shareID)
		return retirement, true
	}
	current.state = routeGrace
	current.graceDeadline = r.now().Add(SenderCrashGrace)
	current.owner = ConnectionRef{}
	return retirement, true
}

type JoinStatus uint8

const (
	JoinReady JoinStatus = iota + 1
	JoinStarting
	JoinNotFound
	JoinStopped
)

type JoinResult struct {
	Status         JoinStatus
	RetryAfter     time.Duration
	RelaySessionID v2.RelaySessionID
	Sender         ConnectionRef
	Descriptor     []byte
}

func (r *Registry) Join(shareID v2.ShareID, receiver ConnectionRef) (JoinResult, error) {
	if r == nil || !receiver.Valid() {
		return JoinResult{}, ErrConfig
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireRoutes(now)
	r.expireSessionTombstones(now)
	current := r.routes[shareID]
	if current == nil {
		return JoinResult{Status: JoinNotFound}, nil
	}
	if current.pendingStop != nil {
		return JoinResult{Status: JoinStarting, RetryAfter: 250 * time.Millisecond}, nil
	}
	switch current.state {
	case routeStarting, routeGrace:
		return JoinResult{Status: JoinStarting, RetryAfter: 250 * time.Millisecond}, nil
	case routeStopped, routeStopUncertain:
		return JoinResult{Status: JoinStopped}, nil
	case routeLive:
	default:
		return JoinResult{}, ErrConfig
	}
	if receiver == current.owner {
		return JoinResult{}, ErrOwner
	}
	if len(r.sessions)+len(r.sessionTombstones) >= r.maxSessions ||
		r.sessionAuthorities[shareID] >= r.maxSessionsPerShare {
		return JoinResult{}, ErrAdmission
	}
	sessionID, err := r.allocateSessionID()
	if err != nil {
		return JoinResult{}, err
	}
	r.sessions[sessionID] = relaySession{shareID: shareID, sender: current.owner, receiver: receiver}
	r.sessionAuthorities[shareID]++
	return JoinResult{
		Status: JoinReady, RelaySessionID: sessionID, Sender: current.owner, Descriptor: bytes.Clone(current.descriptor),
	}, nil
}

// ResolveSession derives direction from the authenticated connection identity.
// A retired result is authority, not an error: only an exact former participant
// may use it to prove that a delayed frame is safe to discard session-locally.
func (r *Registry) ResolveSession(
	sessionID v2.RelaySessionID,
	source ConnectionRef,
) (SessionResolution, error) {
	if r == nil || !source.Valid() {
		return SessionResolution{}, ErrSession
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, exists := r.sessions[sessionID]; exists {
		destination, err := session.destination(source)
		if err != nil {
			return SessionResolution{}, err
		}
		return SessionResolution{Disposition: SessionForward, Destination: destination}, nil
	}
	tombstone, exists := r.sessionTombstones[sessionID]
	if !exists {
		return SessionResolution{}, ErrSession
	}
	if !r.now().Before(tombstone.expiresAt) {
		r.expireSessionTombstone(sessionID, tombstone)
		return SessionResolution{}, ErrSession
	}
	if _, err := tombstone.session.destination(source); err != nil {
		return SessionResolution{}, err
	}
	return SessionResolution{Disposition: SessionRetired}, nil
}

func (session relaySession) destination(source ConnectionRef) (ConnectionRef, error) {
	switch source {
	case session.sender:
		return session.receiver, nil
	case session.receiver:
		return session.sender, nil
	default:
		return ConnectionRef{}, ErrOwner
	}
}

func (r *Registry) EndSession(
	sessionID v2.RelaySessionID,
	source ConnectionRef,
) (SessionRetirement, bool) {
	if r == nil {
		return SessionRetirement{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, exists := r.sessions[sessionID]
	if !exists || (source != session.sender && source != session.receiver) {
		return SessionRetirement{}, false
	}
	return r.retireSession(sessionID, session), true
}

func (r *Registry) allocateSessionID() (v2.RelaySessionID, error) {
	for range sessionIDRetries {
		var id v2.RelaySessionID
		if _, err := io.ReadFull(r.random, id[:]); err != nil {
			return v2.RelaySessionID{}, err
		}
		if allZero(id[:]) {
			continue
		}
		_, active := r.sessions[id]
		_, tombstoned := r.sessionTombstones[id]
		if !active && !tombstoned {
			return id, nil
		}
	}
	return v2.RelaySessionID{}, ErrAdmission
}

func (r *Registry) expireRoutes(now time.Time) {
	for shareID, current := range r.routes {
		switch current.state {
		case routeStarting:
			if current.pendingStop != nil {
				continue
			}
			if !now.Before(current.startDeadline) {
				delete(r.routes, shareID)
			}
		case routeGrace:
			if !now.Before(current.graceDeadline) {
				if current.pendingStop != nil {
					continue
				}
				r.dropShareSessions(shareID)
				delete(r.routes, shareID)
			}
		}
	}
}

func (r *Registry) dropShareSessions(shareID v2.ShareID) []SessionRetirement {
	retirements := make([]SessionRetirement, 0)
	for sessionID, session := range r.sessions {
		if session.shareID == shareID {
			retirements = append(retirements, r.retireSession(sessionID, session))
		}
	}
	slices.SortFunc(retirements, func(left, right SessionRetirement) int {
		return bytes.Compare(left.RelaySessionID[:], right.RelaySessionID[:])
	})
	return retirements
}

func (r *Registry) retireSession(sessionID v2.RelaySessionID, session relaySession) SessionRetirement {
	delete(r.sessions, sessionID)
	r.sessionTombstones[sessionID] = sessionTombstone{
		session: session, expiresAt: r.now().Add(SessionTombstoneTTL),
	}
	return SessionRetirement{RelaySessionID: sessionID, Sender: session.sender, Receiver: session.receiver}
}

func (r *Registry) expireSessionTombstones(now time.Time) {
	for sessionID, tombstone := range r.sessionTombstones {
		if !now.Before(tombstone.expiresAt) {
			r.expireSessionTombstone(sessionID, tombstone)
		}
	}
}

func (r *Registry) expireSessionTombstone(sessionID v2.RelaySessionID, tombstone sessionTombstone) {
	delete(r.sessionTombstones, sessionID)
	shareID := tombstone.session.shareID
	if r.sessionAuthorities[shareID] <= 1 {
		delete(r.sessionAuthorities, shareID)
		return
	}
	r.sessionAuthorities[shareID]--
}

func (r *Registry) retireRoute(current *route, shareID v2.ShareID) RouteRetirement {
	retirement := RouteRetirement{Owner: current.owner, Sessions: r.dropShareSessions(shareID)}
	current.owner = ConnectionRef{}
	current.descriptor = nil
	current.startDeadline = time.Time{}
	current.graceDeadline = time.Time{}
	return retirement
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
