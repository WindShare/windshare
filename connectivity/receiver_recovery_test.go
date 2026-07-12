package connectivity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
)

type fakeRecoverySession struct {
	result chan error
	// closeResult is what Run reports once Close induces teardown; nil models a
	// transfer that was already byte-complete when teardown arrived.
	closeResult error
	once        sync.Once
}

func newFakeRecoverySession() *fakeRecoverySession {
	return &fakeRecoverySession{result: make(chan error, 1), closeResult: session.ErrSessionClosed}
}

func (s *fakeRecoverySession) Run(ctx context.Context) error {
	select {
	case err := <-s.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *fakeRecoverySession) Close() error {
	s.finish(s.closeResult)
	return nil
}

func (s *fakeRecoverySession) finish(err error) {
	s.once.Do(func() { s.result <- err })
}

type fakeRecoveryPool struct {
	mu       sync.Mutex
	channels []session.FrameChannel
	// addErrs are consumed one per AddRelay call; a nil entry admits normally.
	addErrs []error
	added   chan session.FrameChannel
	peer    atomic.Bool
	changes chan struct{}
	closed  atomic.Int32
}

func newFakeRecoveryPool(peer bool) *fakeRecoveryPool {
	pool := &fakeRecoveryPool{
		added:   make(chan session.FrameChannel, 8),
		changes: make(chan struct{}, 8),
	}
	pool.peer.Store(peer)
	return pool
}

func (p *fakeRecoveryPool) AddRelay(channel session.FrameChannel, _ Signaling) error {
	p.mu.Lock()
	if len(p.addErrs) > 0 {
		err := p.addErrs[0]
		p.addErrs = p.addErrs[1:]
		if err != nil {
			p.mu.Unlock()
			return err
		}
	}
	p.channels = append(p.channels, channel)
	p.mu.Unlock()
	p.added <- channel
	return nil
}

func (p *fakeRecoveryPool) PeerAvailable() bool          { return p.peer.Load() }
func (p *fakeRecoveryPool) PeerChanges() <-chan struct{} { return p.changes }
func (p *fakeRecoveryPool) Close() error {
	p.closed.Add(1)
	return nil
}

func (p *fakeRecoveryPool) setPeer(available bool) {
	p.peer.Store(available)
	p.changes <- struct{}{}
}

type fakeReceiverLink struct {
	id        string
	sealed    []byte
	channel   *fakePeerChannel
	signaling Signaling
	done      chan struct{}
	closeOnce sync.Once
	closeCall atomic.Int32

	mu  sync.Mutex
	err error
}

func newFakeReceiverLink(id, sealed string) *fakeReceiverLink {
	return &fakeReceiverLink{
		id:        id,
		sealed:    []byte(sealed),
		channel:   newFakePeerChannel(),
		signaling: &inertSignaling{id: id},
		done:      make(chan struct{}),
	}
}

func (l *fakeReceiverLink) FrameChannel() session.FrameChannel { return l.channel }
func (l *fakeReceiverLink) Signaling() Signaling               { return l.signaling }
func (l *fakeReceiverLink) SealedManifest() []byte             { return append([]byte(nil), l.sealed...) }
func (l *fakeReceiverLink) ID() string                         { return l.id }
func (l *fakeReceiverLink) Done() <-chan struct{}              { return l.done }
func (l *fakeReceiverLink) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *fakeReceiverLink) Close() error {
	l.closeCall.Add(1)
	l.end(nil)
	return nil
}

func (l *fakeReceiverLink) end(err error) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.err = err
		close(l.done)
		l.mu.Unlock()
		l.channel.closeWithError(err)
	})
}

type fakeRecoveryClock struct {
	calls chan time.Duration
	steps chan time.Time
}

func newFakeRecoveryClock() *fakeRecoveryClock {
	return &fakeRecoveryClock{calls: make(chan time.Duration, 8), steps: make(chan time.Time, 8)}
}

func (c *fakeRecoveryClock) After(delay time.Duration) <-chan time.Time {
	c.calls <- delay
	return c.steps
}

func matchingIdentity(want string) ManifestIdentityValidator {
	return ManifestIdentityValidatorFunc(func(sealed []byte) error {
		if string(sealed) != want {
			return errors.New("manifest fingerprint changed")
		}
		return nil
	})
}

func TestReceiverRecoveryKeepsHealthyPeerAfterRelayExhaustion(t *testing.T) {
	wantDialErr := errors.New("relay unavailable")
	sess := newFakeRecoverySession()
	pool := newFakeRecoveryPool(true)
	initial := newFakeReceiverLink("initial", "manifest")
	events := make(chan RecoveryEvent, 8)
	recovery, err := NewReceiverRecovery(
		sess,
		pool,
		ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) { return nil, wantDialErr }),
		matchingIdentity("manifest"),
		ReceiverRecoveryOptions{OnEvent: func(event RecoveryEvent) { events <- event }},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	waitRecoveryEvent(t, events, RecoveryContinuingOnPeer)
	sess.finish(nil)
	if err := waitError(t, result); err != nil {
		t.Fatalf("ReceiverRecovery.Run error = %v", err)
	}
}

func TestReceiverRecoveryRetriesAfterPeerLossUsingInjectedClock(t *testing.T) {
	wantDialErr := errors.New("relay window exhausted")
	sess := newFakeRecoverySession()
	pool := newFakeRecoveryPool(true)
	initial := newFakeReceiverLink("initial", "manifest")
	rejoined := newFakeReceiverLink("rejoined", "manifest")
	clock := newFakeRecoveryClock()
	events := make(chan RecoveryEvent, 8)
	var dialCalls atomic.Int32
	dialer := ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) {
		if dialCalls.Add(1) == 1 {
			return nil, wantDialErr
		}
		return rejoined, nil
	})
	recovery, err := NewReceiverRecovery(
		sess,
		pool,
		dialer,
		matchingIdentity("manifest"),
		ReceiverRecoveryOptions{
			RetryPolicy: RecoveryRetryPolicyFunc(func(retry RecoveryRetry) (time.Duration, bool) {
				if retry.Attempt != 1 || !errors.Is(retry.Err, wantDialErr) {
					t.Errorf("retry = %+v", retry)
				}
				return time.Hour, true
			}),
			Clock:   clock,
			OnEvent: func(event RecoveryEvent) { events <- event },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	waitRecoveryEvent(t, events, RecoveryContinuingOnPeer)
	pool.setPeer(false)
	waitRecoveryEvent(t, events, RecoveryPeerEnded)
	select {
	case delay := <-clock.calls:
		if delay != time.Hour {
			t.Fatalf("retry delay = %s", delay)
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("recovery did not consult injected clock")
	}
	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("dial calls before clock step = %d", got)
	}
	clock.steps <- time.Now()
	waitRecoveryEvent(t, events, RecoveryRelayRestored)
	waitChannel(t, pool.added)
	sess.finish(nil)
	if err := waitError(t, result); err != nil {
		t.Fatalf("ReceiverRecovery.Run error = %v", err)
	}
	if rejoined.closeCall.Load() == 0 {
		t.Fatal("active rejoined link was not closed on completion")
	}
}

func TestReceiverRecoveryTerminalWinsAndClosesLateSuccessfulDial(t *testing.T) {
	wantTerminal := errors.New("terminal domain outcome")
	sess := newFakeRecoverySession()
	pool := newFakeRecoveryPool(false)
	initial := newFakeReceiverLink("initial", "manifest")
	late := newFakeReceiverLink("late", "manifest")
	dialStarted := make(chan struct{})
	dialer := ReceiverDialerFunc(func(ctx context.Context) (ReceiverLink, error) {
		close(dialStarted)
		<-ctx.Done()
		return late, nil
	})
	recovery, err := NewReceiverRecovery(sess, pool, dialer, matchingIdentity("manifest"), ReceiverRecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	select {
	case <-dialStarted:
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("recovery dial did not start")
	}
	sess.finish(wantTerminal)
	if err := waitError(t, result); !errors.Is(err, wantTerminal) {
		t.Fatalf("ReceiverRecovery.Run error = %v", err)
	}
	if late.closeCall.Load() == 0 {
		t.Fatal("late successful dial was not closed before recovery returned")
	}
	select {
	case extra := <-pool.added:
		t.Fatalf("late terminal-racing link was admitted: %T", extra)
	default:
	}
}

// F1 regression: a rejoin admission rejected because the session already
// completed must not convert the byte-complete transfer into an error —
// teardown arbitration must let the nil Run result win over the local cause.
func TestReceiverRecoveryTeardownDoesNotClobberCompletedSession(t *testing.T) {
	sess := newFakeRecoverySession()
	sess.closeResult = nil
	pool := newFakeRecoveryPool(false)
	pool.addErrs = []error{nil, session.ErrSessionClosed}
	initial := newFakeReceiverLink("initial", "manifest")
	rejoined := newFakeReceiverLink("rejoined", "manifest")
	recovery, err := NewReceiverRecovery(
		sess,
		pool,
		ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) { return rejoined, nil }),
		matchingIdentity("manifest"),
		ReceiverRecoveryOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	if err := waitError(t, result); err != nil {
		t.Fatalf("completed transfer was reported as failed: %v", err)
	}
	if rejoined.closeCall.Load() == 0 {
		t.Fatal("rejected rejoin link was not closed")
	}
}

// The success arbitration must not swallow a genuine terminal session error:
// teardown racing a real Run failure still reports that failure, not the
// local teardown cause and not success.
func TestReceiverRecoveryTeardownPropagatesGenuineSessionError(t *testing.T) {
	wantErr := errors.New("terminal session failure")
	sess := newFakeRecoverySession()
	sess.closeResult = wantErr
	pool := newFakeRecoveryPool(false)
	pool.addErrs = []error{nil, session.ErrSessionClosed}
	initial := newFakeReceiverLink("initial", "manifest")
	rejoined := newFakeReceiverLink("rejoined", "manifest")
	recovery, err := NewReceiverRecovery(
		sess,
		pool,
		ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) { return rejoined, nil }),
		matchingIdentity("manifest"),
		ReceiverRecoveryOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	if err := waitError(t, result); !errors.Is(err, wantErr) {
		t.Fatalf("ReceiverRecovery.Run error = %v, want %v", err, wantErr)
	}
}

func TestReceiverRecoveryRejectsManifestDriftBeforeAdmission(t *testing.T) {
	sess := newFakeRecoverySession()
	pool := newFakeRecoveryPool(false)
	initial := newFakeReceiverLink("initial", "manifest")
	drifted := newFakeReceiverLink("drifted", "other-manifest")
	recovery, err := NewReceiverRecovery(
		sess,
		pool,
		ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) { return drifted, nil }),
		matchingIdentity("manifest"),
		ReceiverRecoveryOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- recovery.Run(t.Context(), initial) }()
	waitChannel(t, pool.added)
	initial.end(errors.New("relay severed"))
	if err := waitError(t, result); !errors.Is(err, ErrManifestIdentity) {
		t.Fatalf("ReceiverRecovery.Run error = %v", err)
	}
	if drifted.closeCall.Load() == 0 {
		t.Fatal("identity-rejected link was not closed")
	}
	select {
	case extra := <-pool.added:
		t.Fatalf("identity-rejected link was admitted: %T", extra)
	default:
	}
}

func TestReceiverRecoveryDefaultsAndMissingDependencies(t *testing.T) {
	if _, err := NewReceiverRecovery(nil, newFakeRecoveryPool(false), ReceiverDialerFunc(func(context.Context) (ReceiverLink, error) {
		return nil, nil
	}), matchingIdentity("manifest"), ReceiverRecoveryOptions{}); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("NewReceiverRecovery missing dependency error = %v", err)
	}
	if _, err := NewRelayReceiverLink(nil); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("NewRelayReceiverLink(nil) error = %v", err)
	}
	delay, retry := ImmediateRecoveryRetry().NextDelay(RecoveryRetry{})
	if delay != 0 || !retry {
		t.Fatalf("immediate retry = (%s, %v)", delay, retry)
	}
	select {
	case <-realRecoveryClock{}.After(0):
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("real recovery clock did not fire")
	}
}

func waitRecoveryEvent(t *testing.T, events <-chan RecoveryEvent, want RecoveryEventKind) RecoveryEvent {
	t.Helper()
	deadline := time.After(orchestrationTestTimeout)
	for {
		select {
		case event := <-events:
			if event.Kind == want {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for recovery event %d", want)
			return RecoveryEvent{}
		}
	}
}
