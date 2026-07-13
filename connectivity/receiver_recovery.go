package connectivity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/transport/relay"
)

// ReceiveSession is the scheduler lifecycle consumed by relay recovery.
type ReceiveSession interface {
	Run(context.Context) error
	Close() error
}

// RecoveryPool is the channel-pool projection needed by recovery policy.
type RecoveryPool interface {
	AddRelay(session.FrameChannel, Signaling) error
	PeerAvailable() bool
	PeerChanges() <-chan struct{}
	Close() error
}

// ReceiverLink owns one joined relay session and its signaling adapter.
type ReceiverLink interface {
	FrameChannel() session.FrameChannel
	Signaling() Signaling
	SealedManifest() []byte
	ID() string
	Done() <-chan struct{}
	Err() error
	Close() error
}

type ReceiverDialer interface {
	Dial(context.Context) (ReceiverLink, error)
}

type ReceiverDialerFunc func(context.Context) (ReceiverLink, error)

func (f ReceiverDialerFunc) Dial(ctx context.Context) (ReceiverLink, error) { return f(ctx) }

type ManifestIdentityValidator interface {
	Validate([]byte) error
}

type ManifestIdentityValidatorFunc func([]byte) error

func (f ManifestIdentityValidatorFunc) Validate(sealed []byte) error { return f(sealed) }

type RecoveryRetry struct {
	Attempt int
	Err     error
}

// RecoveryRetryPolicy controls attempts made after an established peer has
// carried the transfer through an exhausted relay-recovery cycle.
type RecoveryRetryPolicy interface {
	NextDelay(RecoveryRetry) (time.Duration, bool)
}

type RecoveryRetryPolicyFunc func(RecoveryRetry) (time.Duration, bool)

func (f RecoveryRetryPolicyFunc) NextDelay(retry RecoveryRetry) (time.Duration, bool) {
	return f(retry)
}

func ImmediateRecoveryRetry() RecoveryRetryPolicy {
	return RecoveryRetryPolicyFunc(func(RecoveryRetry) (time.Duration, bool) { return 0, true })
}

// RecoveryClock keeps retry timing deterministic without forcing relay or CLI
// policy into the receive scheduler.
type RecoveryClock interface {
	After(time.Duration) <-chan time.Time
}

type realRecoveryClock struct{}

func (realRecoveryClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

type RecoveryEventKind uint8

const (
	RecoveryRelayInterrupted RecoveryEventKind = iota
	RecoveryContinuingOnPeer
	RecoveryPeerEnded
	RecoveryRelayRestored
)

type RecoveryEvent struct {
	Kind       RecoveryEventKind
	Err        error
	ReceiverID string
}

type ReceiverRecoveryOptions struct {
	RetryPolicy RecoveryRetryPolicy
	Clock       RecoveryClock
	OnEvent     func(RecoveryEvent)
}

// ReceiverRecovery owns relay redial, terminal-versus-redial arbitration,
// manifest-gated admission, established-peer continuation, and late-dial cleanup.
type ReceiverRecovery struct {
	session  ReceiveSession
	pool     RecoveryPool
	dialer   ReceiverDialer
	validate ManifestIdentityValidator
	retry    RecoveryRetryPolicy
	clock    RecoveryClock
	onEvent  func(RecoveryEvent)
}

func NewReceiverRecovery(
	receiveSession ReceiveSession,
	pool RecoveryPool,
	dialer ReceiverDialer,
	validator ManifestIdentityValidator,
	options ReceiverRecoveryOptions,
) (*ReceiverRecovery, error) {
	if receiveSession == nil || pool == nil || dialer == nil || validator == nil {
		return nil, fmt.Errorf("%w: receive session, receiver pool, relay dialer, and manifest validator are required", ErrNilDependency)
	}
	if options.RetryPolicy == nil {
		options.RetryPolicy = ImmediateRecoveryRetry()
	}
	if options.Clock == nil {
		options.Clock = realRecoveryClock{}
	}
	if options.OnEvent == nil {
		options.OnEvent = func(RecoveryEvent) {}
	}
	return &ReceiverRecovery{
		session:  receiveSession,
		pool:     pool,
		dialer:   dialer,
		validate: validator,
		retry:    options.RetryPolicy,
		clock:    options.Clock,
		onEvent:  options.OnEvent,
	}, nil
}

func (r *ReceiverRecovery) Run(ctx context.Context, initial ReceiverLink) error {
	if ctx == nil || initial == nil {
		return fmt.Errorf("%w: context and initial relay link are required", ErrNilDependency)
	}
	if err := r.admitInitialLink(initial); err != nil {
		return err
	}
	current := initial
	defer func() {
		_ = r.pool.Close()
		_ = current.Close()
	}()

	sessionDone := make(chan error, 1)
	go func() { sessionDone <- r.session.Run(ctx) }()

	for {
		if ended, sessionErr := r.awaitRelayInterruption(ctx, sessionDone, current); ended {
			return sessionErr
		}
		next, ended, terminalErr := r.restoreRelay(ctx, sessionDone)
		if ended {
			return terminalErr
		}
		_ = current.Close()
		current = next
		r.onEvent(RecoveryEvent{Kind: RecoveryRelayRestored, ReceiverID: next.ID()})
	}
}

func (r *ReceiverRecovery) admitInitialLink(initial ReceiverLink) error {
	if err := r.validate.Validate(initial.SealedManifest()); err != nil {
		_ = initial.Close()
		_ = r.pool.Close()
		return fmt.Errorf("%w: %w", ErrManifestIdentity, err)
	}
	if err := r.pool.AddRelay(initial.FrameChannel(), initial.Signaling()); err != nil {
		_ = initial.Close()
		_ = r.pool.Close()
		return err
	}
	return nil
}

// awaitRelayInterruption blocks until the relay link drops, the session ends,
// or the caller cancels. A session result racing the relay drop must win so a
// completed transfer is never diverted into recovery.
func (r *ReceiverRecovery) awaitRelayInterruption(
	ctx context.Context,
	sessionDone <-chan error,
	current ReceiverLink,
) (bool, error) {
	select {
	case sessionErr := <-sessionDone:
		return true, sessionErr
	case <-ctx.Done():
		return true, r.settleSession(sessionDone, ctx.Err())
	case <-current.Done():
		select {
		case sessionErr := <-sessionDone:
			return true, sessionErr
		default:
		}
		r.onEvent(RecoveryEvent{Kind: RecoveryRelayInterrupted, Err: current.Err(), ReceiverID: current.ID()})
		return false, nil
	}
}

// restoreRelay redials until a manifest-validated link is admitted to the pool
// or a terminal outcome ends the run. The attempt counter is scoped to one
// recovery cycle: the sole non-terminal exit is a successful restore, which is
// exactly where the original counter reset.
func (r *ReceiverRecovery) restoreRelay(
	ctx context.Context,
	sessionDone <-chan error,
) (ReceiverLink, bool, error) {
	failedAttempts := 0
	for {
		next, sessionErr, sessionEnded, dialErr := r.dialAgainstSession(ctx, sessionDone)
		if sessionEnded {
			return nil, true, sessionErr
		}
		if dialErr != nil {
			failedAttempts++
			if ended, terminalErr := r.holdForRetry(ctx, sessionDone, next, failedAttempts, dialErr); ended {
				return nil, true, terminalErr
			}
			continue
		}
		if ended, terminalErr := r.admitRestoredLink(sessionDone, next); ended {
			return nil, true, terminalErr
		}
		return next, false, nil
	}
}

// holdForRetry rides out a failed dial on the established peer, then applies
// the retry policy once that peer ends. Without a peer the transfer has no
// remaining path, so the recovery error becomes the settlement cause.
func (r *ReceiverRecovery) holdForRetry(
	ctx context.Context,
	sessionDone <-chan error,
	failed ReceiverLink,
	attempt int,
	dialErr error,
) (bool, error) {
	if failed != nil {
		_ = failed.Close()
	}
	recoveryErr := fmt.Errorf("%w: %w", ErrRelayRecoveryFailed, dialErr)
	if !r.pool.PeerAvailable() {
		return true, r.settleSession(sessionDone, recoveryErr)
	}
	r.onEvent(RecoveryEvent{Kind: RecoveryContinuingOnPeer, Err: recoveryErr})
	if ended, sessionErr := r.waitForPeerOrSession(ctx, sessionDone); ended {
		return true, sessionErr
	}
	r.onEvent(RecoveryEvent{Kind: RecoveryPeerEnded, Err: recoveryErr})
	delay, retry := r.retry.NextDelay(RecoveryRetry{Attempt: attempt, Err: recoveryErr})
	if !retry {
		return true, r.settleSession(sessionDone, recoveryErr)
	}
	select {
	case sessionErr := <-sessionDone:
		return true, sessionErr
	case <-ctx.Done():
		return true, r.settleSession(sessionDone, ctx.Err())
	case <-r.clock.After(delay):
	}
	return false, nil
}

// admitRestoredLink gates the redialed link on manifest identity and a final
// session-completion check before handing its channel to the pool.
func (r *ReceiverRecovery) admitRestoredLink(sessionDone <-chan error, next ReceiverLink) (bool, error) {
	if err := r.validate.Validate(next.SealedManifest()); err != nil {
		_ = next.Close()
		return true, r.settleSession(sessionDone, fmt.Errorf("%w: %w", ErrManifestIdentity, err))
	}
	select {
	case sessionErr := <-sessionDone:
		_ = next.Close()
		return true, sessionErr
	default:
	}
	if err := r.pool.AddRelay(next.FrameChannel(), next.Signaling()); err != nil {
		_ = next.Close()
		return true, r.settleSession(sessionDone, err)
	}
	return false, nil
}

// settleSession tears down the scheduler and arbitrates its result against the
// local cause that triggered teardown. A nil Run result means the transfer is
// already byte-complete, so success must win even when teardown races the
// completing session — otherwise a finished transfer is misreported and the
// caller skips finalization. ErrSessionClosed means Run ended only because of
// this induced Close, so the local cause is the real outcome; any other
// session error is a genuine terminal result and takes precedence.
func (r *ReceiverRecovery) settleSession(sessionDone <-chan error, cause error) error {
	_ = r.session.Close()
	sessionErr := <-sessionDone
	if sessionErr == nil {
		return nil
	}
	if errors.Is(sessionErr, session.ErrSessionClosed) {
		return cause
	}
	return sessionErr
}

func (r *ReceiverRecovery) dialAgainstSession(
	ctx context.Context,
	sessionDone <-chan error,
) (ReceiverLink, error, bool, error) {
	type dialResult struct {
		link ReceiverLink
		err  error
	}
	dialCtx, cancel := context.WithCancel(ctx)
	results := make(chan dialResult, 1)
	go func() {
		link, err := r.dialer.Dial(dialCtx)
		results <- dialResult{link: link, err: err}
	}()
	settleDial := func() dialResult {
		cancel()
		result := <-results
		if result.link != nil {
			_ = result.link.Close()
		}
		return result
	}
	select {
	case sessionErr := <-sessionDone:
		settleDial()
		return nil, sessionErr, true, nil
	case <-ctx.Done():
		settleDial()
		return nil, r.settleSession(sessionDone, ctx.Err()), true, nil
	case result := <-results:
		cancel()
		select {
		case sessionErr := <-sessionDone:
			if result.link != nil {
				_ = result.link.Close()
			}
			return nil, sessionErr, true, nil
		default:
		}
		if result.link == nil && result.err == nil {
			result.err = fmt.Errorf("%w: relay dialer returned a nil link", ErrNilDependency)
		}
		return result.link, nil, false, result.err
	}
}

func (r *ReceiverRecovery) waitForPeerOrSession(
	ctx context.Context,
	sessionDone <-chan error,
) (bool, error) {
	for r.pool.PeerAvailable() {
		select {
		case sessionErr := <-sessionDone:
			return true, sessionErr
		case <-ctx.Done():
			return true, r.settleSession(sessionDone, ctx.Err())
		case <-r.pool.PeerChanges():
		}
	}
	select {
	case sessionErr := <-sessionDone:
		return true, sessionErr
	default:
		return false, nil
	}
}

type relayReceiverLink struct {
	conn      *relay.ReceiverConn
	signaling Signaling
}

func NewRelayReceiverLink(conn *relay.ReceiverConn) (ReceiverLink, error) {
	if conn == nil {
		return nil, fmt.Errorf("%w: relay receiver connection", ErrNilDependency)
	}
	signaling, err := NewRelaySignaling(conn.Channel())
	if err != nil {
		return nil, err
	}
	return &relayReceiverLink{conn: conn, signaling: signaling}, nil
}

func (l *relayReceiverLink) FrameChannel() session.FrameChannel { return l.conn.Channel() }
func (l *relayReceiverLink) Signaling() Signaling               { return l.signaling }
func (l *relayReceiverLink) SealedManifest() []byte             { return l.conn.SealedManifest() }
func (l *relayReceiverLink) ID() string                         { return l.conn.SessionID().String() }
func (l *relayReceiverLink) Done() <-chan struct{}              { return l.conn.Done() }
func (l *relayReceiverLink) Err() error                         { return l.conn.Err() }
func (l *relayReceiverLink) Close() error                       { return l.conn.Close() }

type RelayReceiverDialer struct{ config relay.ReceiverConfig }

func NewRelayReceiverDialer(config relay.ReceiverConfig) *RelayReceiverDialer {
	return &RelayReceiverDialer{config: config}
}

func (d *RelayReceiverDialer) Dial(ctx context.Context) (ReceiverLink, error) {
	conn, err := relay.DialReceiver(ctx, d.config)
	if err != nil {
		return nil, err
	}
	link, err := NewRelayReceiverLink(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return link, nil
}
