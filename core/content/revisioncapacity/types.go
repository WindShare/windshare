package revisioncapacity

import (
	"errors"
	"fmt"
	"time"
)

const (
	DefaultSessionActiveLeases uint64 = 32
	DefaultShareActiveLeases   uint64 = 256
	DefaultProcessActiveLeases uint64 = 1_024

	DefaultSessionStableHandles uint64 = 32
	DefaultShareStableHandles   uint64 = 256
	DefaultProcessStableHandles uint64 = 1_024

	MinCapacityRetryAfter     = time.Millisecond
	MaxCapacityRetryAfter     = 30 * time.Second
	DefaultCapacityRetryAfter = 250 * time.Millisecond
)

var (
	ErrCoordinatorClosed     = errors.New("revision capacity coordinator is closed")
	ErrRegistrationClosing   = errors.New("revision capacity registration is closing")
	ErrOwnershipResolved     = errors.New("revision capacity ownership is already resolved")
	ErrOwnershipClaimed      = errors.New("revision capacity ownership has an active reclaim claim")
	ErrOwnershipQuarantined  = errors.New("revision capacity ownership is quarantined")
	ErrInvalidReclaimResult  = errors.New("revision capacity reclaim target violated its ownership contract")
	ErrAdmissionGrantSettled = errors.New("revision capacity admission grant is already settled")
)

type StoreID string
type ShareID string
type SessionID string
type RevisionID string
type CandidateToken string
type CapacityDecisionID string
type ReclaimClaimID string

type CapacityUsage struct {
	StableHandles uint64
	ActiveLeases  uint64
}

type CapacityLimits CapacityUsage

func DefaultProcessLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultProcessStableHandles, ActiveLeases: DefaultProcessActiveLeases}
}

func DefaultShareLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultShareStableHandles, ActiveLeases: DefaultShareActiveLeases}
}

func DefaultSessionLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultSessionStableHandles, ActiveLeases: DefaultSessionActiveLeases}
}

type ProcessConfig struct {
	Limits     CapacityLimits
	RetryAfter time.Duration
	Tracer     Tracer
}

func DefaultProcessConfig() ProcessConfig {
	return ProcessConfig{Limits: DefaultProcessLimits(), RetryAfter: DefaultCapacityRetryAfter}
}

type StoreConfig struct {
	StoreID StoreID
	ShareID ShareID
	Limits  CapacityLimits
}

type SessionConfig struct {
	SessionID SessionID
	Limits    CapacityLimits
}

type AdmissionKind uint8

const (
	AdmissionNewRevision AdmissionKind = iota + 1
	AdmissionFirstSessionLease
	AdmissionAdditionalSessionLease
)

type AdmissionRequest struct {
	Kind       AdmissionKind
	RevisionID RevisionID
	Session    *SessionRegistration
}

type CapacityResource uint8

const (
	CapacityResourceStableHandle CapacityResource = iota + 1
	CapacityResourceActiveLease
)

func (r CapacityResource) String() string {
	switch r {
	case CapacityResourceStableHandle:
		return "stable_handle"
	case CapacityResourceActiveLease:
		return "active_lease"
	default:
		return "unknown"
	}
}

type CapacityScope uint8

const (
	CapacityScopeProcess CapacityScope = iota + 1
	CapacityScopeShare
	CapacityScopeSession
)

func (s CapacityScope) String() string {
	switch s {
	case CapacityScopeProcess:
		return "process"
	case CapacityScopeShare:
		return "share"
	case CapacityScopeSession:
		return "session"
	default:
		return "unknown"
	}
}

type ScopeSnapshot struct {
	scope       CapacityScope
	identity    string
	limits      CapacityLimits
	used        CapacityUsage
	reclaimable uint64
	quarantined uint64
	pending     uint64
	reclaims    uint64
}

func (s ScopeSnapshot) Scope() CapacityScope             { return s.scope }
func (s ScopeSnapshot) Identity() string                 { return s.identity }
func (s ScopeSnapshot) Limits() CapacityLimits           { return s.limits }
func (s ScopeSnapshot) Used() CapacityUsage              { return s.used }
func (s ScopeSnapshot) ReclaimableStableHandles() uint64 { return s.reclaimable }
func (s ScopeSnapshot) QuarantinedStableHandles() uint64 { return s.quarantined }
func (s ScopeSnapshot) PendingAdmissions() uint64        { return s.pending }
func (s ScopeSnapshot) ActiveReclaims() uint64           { return s.reclaims }

type CapacitySnapshot struct {
	process  ScopeSnapshot
	share    ScopeSnapshot
	sessions []ScopeSnapshot
	closed   bool
}

func (s CapacitySnapshot) Process() ScopeSnapshot { return s.process }
func (s CapacitySnapshot) Share() ScopeSnapshot   { return s.share }
func (s CapacitySnapshot) Closed() bool           { return s.closed }
func (s CapacitySnapshot) Sessions() []ScopeSnapshot {
	return append([]ScopeSnapshot(nil), s.sessions...)
}

type CapacityBusyError struct {
	decisionID CapacityDecisionID
	resource   CapacityResource
	scope      CapacityScope
	retryAfter time.Duration
	snapshot   CapacitySnapshot
}

func (e *CapacityBusyError) Error() string {
	if e == nil {
		return "revision capacity is busy"
	}
	return fmt.Sprintf("revision capacity is busy: %s limit at %s scope (decision %s)", e.resource, e.scope, e.decisionID)
}

func (e *CapacityBusyError) DecisionID() CapacityDecisionID { return e.decisionID }
func (e *CapacityBusyError) Resource() CapacityResource     { return e.resource }
func (e *CapacityBusyError) Scope() CapacityScope           { return e.scope }
func (e *CapacityBusyError) RetryAfter() time.Duration      { return e.retryAfter }
func (e *CapacityBusyError) Snapshot() CapacitySnapshot     { return cloneSnapshot(e.snapshot) }

type LiveStoreRegistrationsError struct {
	count int
}

func (e *LiveStoreRegistrationsError) Error() string {
	return fmt.Sprintf("revision capacity process owner has %d live store registration(s)", e.count)
}

func (e *LiveStoreRegistrationsError) Count() int { return e.count }

type LiveSessionRegistrationsError struct {
	storeID StoreID
	count   int
}

func (e *LiveSessionRegistrationsError) Error() string {
	return fmt.Sprintf("revision capacity store %q has %d live session registration(s)", e.storeID, e.count)
}

func (e *LiveSessionRegistrationsError) StoreID() StoreID { return e.storeID }
func (e *LiveSessionRegistrationsError) Count() int       { return e.count }

type LiveCapacityOwnershipError struct {
	scope    CapacityScope
	identity string
	usage    CapacityUsage
}

func (e *LiveCapacityOwnershipError) Error() string {
	return fmt.Sprintf("revision capacity %s %q still owns capacity %+v", e.scope, e.identity, e.usage)
}

func (e *LiveCapacityOwnershipError) Scope() CapacityScope { return e.scope }
func (e *LiveCapacityOwnershipError) Identity() string     { return e.identity }
func (e *LiveCapacityOwnershipError) Usage() CapacityUsage { return e.usage }

type ReclaimOwnershipError struct {
	decisionID CapacityDecisionID
	claimID    ReclaimClaimID
	candidate  CandidateToken
	cause      error
}

func (e *ReclaimOwnershipError) Error() string {
	return fmt.Sprintf("revision capacity reclaim ownership is uncertain for candidate %q (decision %s): %v", e.candidate, e.decisionID, e.cause)
}

func (e *ReclaimOwnershipError) Unwrap() error                  { return e.cause }
func (e *ReclaimOwnershipError) DecisionID() CapacityDecisionID { return e.decisionID }
func (e *ReclaimOwnershipError) ClaimID() ReclaimClaimID        { return e.claimID }
func (e *ReclaimOwnershipError) CandidateToken() CandidateToken { return e.candidate }

type TraceStage uint8

const (
	TraceAdmissionGranted TraceStage = iota + 1
	TraceAdmissionDenied
	TraceAdmissionCancelled
	TraceReclaimClaimed
	TraceReclaimDeclined
	TraceReclaimCompleted
	TraceReclaimQuarantined
	TraceIdlePublished
	TraceIdleWithdrawn
	TraceOwnershipQuarantined
)

type TraceEvent struct {
	stage      TraceStage
	decision   CapacityDecisionID
	claim      ReclaimClaimID
	storeID    StoreID
	shareID    ShareID
	sessionID  SessionID
	revision   RevisionID
	candidate  CandidateToken
	snapshot   CapacitySnapshot
	diagnostic error
}

func (e TraceEvent) Stage() TraceStage              { return e.stage }
func (e TraceEvent) DecisionID() CapacityDecisionID { return e.decision }
func (e TraceEvent) ClaimID() ReclaimClaimID        { return e.claim }
func (e TraceEvent) StoreID() StoreID               { return e.storeID }
func (e TraceEvent) ShareID() ShareID               { return e.shareID }
func (e TraceEvent) SessionID() SessionID           { return e.sessionID }
func (e TraceEvent) RevisionID() RevisionID         { return e.revision }
func (e TraceEvent) CandidateToken() CandidateToken { return e.candidate }
func (e TraceEvent) Snapshot() CapacitySnapshot     { return cloneSnapshot(e.snapshot) }
func (e TraceEvent) Diagnostic() error              { return e.diagnostic }

type Tracer interface {
	TraceCapacity(TraceEvent)
}

type TracerFunc func(TraceEvent)

func (f TracerFunc) TraceCapacity(event TraceEvent) { f(event) }

func validateLimits(limits CapacityLimits) error {
	if limits.StableHandles == 0 || limits.ActiveLeases == 0 {
		return errors.New("revision capacity limits must be positive")
	}
	return nil
}

func validateRetryAfter(delay time.Duration) error {
	if delay < MinCapacityRetryAfter || delay > MaxCapacityRetryAfter || delay%time.Millisecond != 0 {
		return errors.New("revision capacity retry delay must be an integral millisecond within protocol bounds")
	}
	return nil
}

func cloneSnapshot(snapshot CapacitySnapshot) CapacitySnapshot {
	snapshot.sessions = append([]ScopeSnapshot(nil), snapshot.sessions...)
	return snapshot
}
