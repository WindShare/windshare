package content

import (
	"errors"
	"fmt"
	"strconv"
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
	ErrHandlePoolClosed = errors.New("revision handle pool is closed")
	ErrSessionClosed    = errors.New("revision session is closed")
)

type SessionID string
type CapacityDecisionID string

type CapacityUsage struct {
	StableHandles uint64
	ActiveLeases  uint64
}

type CapacityLimits CapacityUsage

func DefaultProcessCapacityLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultProcessStableHandles, ActiveLeases: DefaultProcessActiveLeases}
}

func DefaultShareCapacityLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultShareStableHandles, ActiveLeases: DefaultShareActiveLeases}
}

func DefaultSessionCapacityLimits() CapacityLimits {
	return CapacityLimits{StableHandles: DefaultSessionStableHandles, ActiveLeases: DefaultSessionActiveLeases}
}

type HandlePoolConfig struct {
	Limits     CapacityLimits
	RetryAfter time.Duration
	Tracer     CapacityTracer
}

func DefaultHandlePoolConfig() HandlePoolConfig {
	return HandlePoolConfig{Limits: DefaultProcessCapacityLimits(), RetryAfter: DefaultCapacityRetryAfter}
}

type SessionConfig struct {
	SessionID SessionID
	Limits    CapacityLimits
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
}

func (s ScopeSnapshot) Scope() CapacityScope             { return s.scope }
func (s ScopeSnapshot) Identity() string                 { return s.identity }
func (s ScopeSnapshot) Limits() CapacityLimits           { return s.limits }
func (s ScopeSnapshot) Used() CapacityUsage              { return s.used }
func (s ScopeSnapshot) ReclaimableStableHandles() uint64 { return s.reclaimable }

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

func NewCapacityBusyError(
	decisionID CapacityDecisionID,
	resource CapacityResource,
	scope CapacityScope,
	retryAfter time.Duration,
) *CapacityBusyError {
	return &CapacityBusyError{decisionID: decisionID, resource: resource, scope: scope, retryAfter: retryAfter}
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
func (e *CapacityBusyError) Snapshot() CapacitySnapshot {
	snapshot := e.snapshot
	snapshot.sessions = append([]ScopeSnapshot(nil), snapshot.sessions...)
	return snapshot
}

type LiveStoresError struct {
	count int
}

func (e *LiveStoresError) Error() string {
	return fmt.Sprintf("revision handle pool has %d live store(s)", e.count)
}

func (e *LiveStoresError) Count() int { return e.count }

type CapacityTraceStage uint8

const (
	CapacityTraceAdmitted CapacityTraceStage = iota + 1
	CapacityTraceDenied
	CapacityTraceEvicted
)

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value CapacityTraceStage) DiagnosticName() string {
	switch value {
	case CapacityTraceAdmitted:
		return "admission_granted"
	case CapacityTraceDenied:
		return "admission_denied"
	case CapacityTraceEvicted:
		return "idle_evicted"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

type CapacityTraceEvent struct {
	stage      CapacityTraceStage
	decision   CapacityDecisionID
	shareID    string
	sessionID  SessionID
	revisionID string
	snapshot   CapacitySnapshot
	diagnostic error
}

func (e CapacityTraceEvent) Stage() CapacityTraceStage      { return e.stage }
func (e CapacityTraceEvent) DecisionID() CapacityDecisionID { return e.decision }
func (e CapacityTraceEvent) ShareID() string                { return e.shareID }
func (e CapacityTraceEvent) SessionID() SessionID           { return e.sessionID }
func (e CapacityTraceEvent) RevisionID() string             { return e.revisionID }
func (e CapacityTraceEvent) Snapshot() CapacitySnapshot {
	snapshot := e.snapshot
	snapshot.sessions = append([]ScopeSnapshot(nil), snapshot.sessions...)
	return snapshot
}
func (e CapacityTraceEvent) Diagnostic() error { return e.diagnostic }

type CapacityTracer interface {
	TraceCapacity(CapacityTraceEvent)
}

type CapacityTracerFunc func(CapacityTraceEvent)

func (f CapacityTracerFunc) TraceCapacity(event CapacityTraceEvent) { f(event) }

func validateCapacityLimits(limits CapacityLimits) error {
	if limits.StableHandles == 0 || limits.ActiveLeases == 0 {
		return errors.New("revision capacity limits must be positive")
	}
	return nil
}

func validateCapacityRetryAfter(delay time.Duration) error {
	if delay < MinCapacityRetryAfter || delay > MaxCapacityRetryAfter || delay%time.Millisecond != 0 {
		return errors.New("revision capacity retry delay must be an integral millisecond within protocol bounds")
	}
	return nil
}
