// Package revisionwait owns receiver-side scheduling for authenticated revision
// capacity pressure. It deliberately does not know how protocol responses are
// authenticated; session adapters construct CapacitySignal only after that proof.
package revisionwait

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MinimumRetryHint = time.Millisecond
	MaximumRetryHint = 30 * time.Second
	GenerationBytes  = protocolsession.IdentityBytes
)

var (
	ErrInvalidCapacitySignal  = errors.New("revision capacity signal is invalid")
	ErrInvalidGenerationToken = errors.New("revision wait generation token is invalid")
	ErrGenerationEnded        = errors.New("revision wait protocol generation ended")
	ErrGenerationChanged      = errors.New("revision wait protocol generation changed")
)

// GenerationToken identifies one native protocol runtime lifetime. It is kept
// opaque so a future reconnect supervisor can replace the fixed-runtime fence
// without teaching transfer scheduling how sessions are assembled.
type GenerationToken [GenerationBytes]byte

func GenerationTokenFromBytes(raw []byte) (GenerationToken, error) {
	var token GenerationToken
	if len(raw) != len(token) {
		return token, ErrInvalidGenerationToken
	}
	copy(token[:], raw)
	if token.IsZero() {
		return GenerationToken{}, ErrInvalidGenerationToken
	}
	return token, nil
}

func (token GenerationToken) Bytes() []byte {
	result := make([]byte, len(token))
	copy(result, token[:])
	return result
}

func (token GenerationToken) IsZero() bool { return token == GenerationToken{} }

type CapacitySignalSpec struct {
	RetryAfter        time.Duration
	ProtocolSession   protocolsession.ProtocolSessionID
	ProtocolOperation protocolsession.OperationID
	Generation        GenerationToken
}

// CapacitySignal is scheduling authority only when returned directly by the
// authenticated session adapter. MatchCapacitySignal intentionally refuses
// wrappers so a diagnostic error graph cannot acquire retry authority.
type CapacitySignal struct {
	retryAfter        time.Duration
	protocolSession   protocolsession.ProtocolSessionID
	protocolOperation protocolsession.OperationID
	generation        GenerationToken
}

func NewCapacitySignal(spec CapacitySignalSpec) (*CapacitySignal, error) {
	if spec.RetryAfter < MinimumRetryHint || spec.RetryAfter > MaximumRetryHint ||
		spec.RetryAfter%time.Millisecond != 0 ||
		spec.ProtocolSession.IsZero() || spec.ProtocolOperation.IsZero() || spec.Generation.IsZero() {
		return nil, ErrInvalidCapacitySignal
	}
	return &CapacitySignal{
		retryAfter: spec.RetryAfter, protocolSession: spec.ProtocolSession,
		protocolOperation: spec.ProtocolOperation, generation: spec.Generation,
	}, nil
}

func (signal *CapacitySignal) Error() string {
	if signal == nil {
		return ErrInvalidCapacitySignal.Error()
	}
	return fmt.Sprintf("revision capacity is busy; retry after %s", signal.retryAfter)
}

func (signal *CapacitySignal) RetryAfter() time.Duration { return signal.retryAfter }
func (signal *CapacitySignal) ProtocolSession() protocolsession.ProtocolSessionID {
	return signal.protocolSession
}
func (signal *CapacitySignal) ProtocolOperation() protocolsession.OperationID {
	return signal.protocolOperation
}
func (signal *CapacitySignal) Generation() GenerationToken { return signal.generation }

func (signal *CapacitySignal) valid() bool {
	return signal != nil && signal.retryAfter >= MinimumRetryHint && signal.retryAfter <= MaximumRetryHint &&
		signal.retryAfter%time.Millisecond == 0 &&
		!signal.protocolSession.IsZero() && !signal.protocolOperation.IsZero() && !signal.generation.IsZero()
}

func MatchCapacitySignal(err error) (*CapacitySignal, bool) {
	var signal *CapacitySignal
	// Scheduling authority is an exact boundary value, not a property that an
	// arbitrary diagnostic wrapper may inherit through its error graph.
	direct := reflect.TypeOf(err) == reflect.TypeOf(signal) && errors.As(err, &signal)
	return signal, direct && signal.valid()
}

type GenerationChangeKind uint8

const (
	GenerationReplaced GenerationChangeKind = iota + 1
	GenerationLifetimeEnded
)

type GenerationChange struct {
	kind     GenerationChangeKind
	previous GenerationToken
	current  GenerationToken
	cause    error
}

func NewGenerationReplacement(previous, current GenerationToken, cause error) (GenerationChange, error) {
	if previous.IsZero() || current.IsZero() || previous == current {
		return GenerationChange{}, ErrInvalidGenerationToken
	}
	if cause == nil {
		cause = ErrGenerationChanged
	}
	return GenerationChange{kind: GenerationReplaced, previous: previous, current: current, cause: cause}, nil
}

func NewGenerationEnd(previous GenerationToken, cause error) (GenerationChange, error) {
	if previous.IsZero() {
		return GenerationChange{}, ErrInvalidGenerationToken
	}
	if cause == nil {
		cause = ErrGenerationEnded
	}
	return GenerationChange{kind: GenerationLifetimeEnded, previous: previous, cause: cause}, nil
}

func (change GenerationChange) Kind() GenerationChangeKind { return change.kind }
func (change GenerationChange) Previous() GenerationToken  { return change.previous }
func (change GenerationChange) Current() GenerationToken   { return change.current }
func (change GenerationChange) Cause() error               { return change.cause }

func (change GenerationChange) validFor(expected GenerationToken) bool {
	if expected.IsZero() || change.previous != expected || change.cause == nil {
		return false
	}
	switch change.kind {
	case GenerationReplaced:
		return !change.current.IsZero() && change.current != expected
	case GenerationLifetimeEnded:
		return change.current.IsZero()
	default:
		return false
	}
}

type GenerationFence interface {
	Current() GenerationToken
	WaitForChange(context.Context, GenerationToken) (GenerationChange, error)
}

// LifetimeFence turns a runtime lifecycle into the fixed-generation contract
// native transfer has today. Ending the runtime wakes every retry waiter, while
// a later runtime necessarily creates a new token and a new transfer job.
type LifetimeFence struct {
	token     GenerationToken
	lifecycle context.Context
}

func NewLifetimeFence(token GenerationToken, lifecycle context.Context) (*LifetimeFence, error) {
	if token.IsZero() || lifecycle == nil {
		return nil, ErrInvalidGenerationToken
	}
	return &LifetimeFence{token: token, lifecycle: lifecycle}, nil
}

func (fence *LifetimeFence) Current() GenerationToken {
	if fence == nil || fence.lifecycle == nil || fence.lifecycle.Err() != nil {
		return GenerationToken{}
	}
	return fence.token
}

func (fence *LifetimeFence) WaitForChange(
	ctx context.Context,
	expected GenerationToken,
) (GenerationChange, error) {
	if fence == nil || fence.lifecycle == nil || expected.IsZero() {
		return GenerationChange{}, ErrInvalidGenerationToken
	}
	if expected != fence.token {
		current := fence.Current()
		if current.IsZero() {
			return NewGenerationEnd(expected, context.Cause(fence.lifecycle))
		}
		return NewGenerationReplacement(expected, current, ErrGenerationChanged)
	}
	select {
	case <-ctx.Done():
		return GenerationChange{}, context.Cause(ctx)
	case <-fence.lifecycle.Done():
		return NewGenerationEnd(expected, context.Cause(fence.lifecycle))
	}
}
