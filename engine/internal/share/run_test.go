package share

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/transport/relayv2"
)

const testWaitTimeout = 5 * time.Second

type shareFixture struct {
	mu           sync.Mutex
	steps        []string
	facts        []Observation
	prepared     *testPrepared
	relays       *testRelays
	factory      *testSessionFactory
	dependencies Dependencies
}

func newShareFixture() *shareFixture {
	f := &shareFixture{}
	f.relays = &testRelays{step: f.step, stopped: make(chan struct{})}
	f.factory = &testSessionFactory{relays: f.relays, step: f.step}
	f.prepared = &testPrepared{factory: f.factory, step: f.step}
	f.dependencies = Dependencies{
		Controller: NewController(),
		Control: task.Control{
			ID: "share-test", Now: time.Now, Random: rand.Reader,
			CleanupContext: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), testWaitTimeout)
			},
			Emit: func(event task.Event) bool {
				if value, ok := event.(Observation); ok {
					f.mu.Lock()
					f.facts = append(f.facts, value)
					f.mu.Unlock()
				}
				return true
			},
		},
		Prepare: func(context.Context, liveshare.SenderConfig) (Prepared, error) {
			f.step("prepare")
			return f.prepared, nil
		},
		Relays: func(context.Context, []string, relayset.SenderFactory) (Relays, error) {
			f.step("relays")
			return f.relays, nil
		},
		PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done(); f.step("network_joined") },
	}
	return f
}
func (f *shareFixture) step(value string) {
	f.mu.Lock()
	f.steps = append(f.steps, value)
	f.mu.Unlock()
}
func (f *shareFixture) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steps...)
}

type testPrepared struct {
	factory                            SessionFactory
	step                               func(string)
	authorizeErr, runtimeErr, closeErr error
	observeRuntime                     func(liveshare.RuntimeFactoryConfig)
}

func (p *testPrepared) AuthorizeRegistration() error { p.step("authorize"); return p.authorizeErr }
func (*testPrepared) Registration() liveshare.RegistrationMaterial {
	return liveshare.RegistrationMaterial{}
}
func (*testPrepared) Capability() link.Link { return link.Link{} }
func (*testPrepared) SelectedRootSummary() liveshare.SelectedRootSummary {
	return liveshare.SelectedRootSummary{}
}
func (p *testPrepared) NewRuntimeFactory(config liveshare.RuntimeFactoryConfig) (SessionFactory, error) {
	p.step("factory")
	if p.observeRuntime != nil {
		p.observeRuntime(config)
	}
	return p.factory, p.runtimeErr
}
func (p *testPrepared) StartRootPrefetch() { p.step("prefetch") }
func (p *testPrepared) Close() error       { p.step("source_closed"); return p.closeErr }

type testRelays struct {
	step                           func(string)
	waitErr, acceptErr, cleanupErr error
	wait                           func(context.Context) error
	stopped                        chan struct{}
	once                           sync.Once
}

func (r *testRelays) Accept(ctx context.Context) (*relayv2.Channel, error) {
	if r.acceptErr != nil {
		return nil, r.acceptErr
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-r.stopped:
		return nil, context.Canceled
	}
}
func (r *testRelays) WaitReady(ctx context.Context) error {
	if r.wait != nil {
		return r.wait(ctx)
	}
	return r.waitErr
}
func (*testRelays) ReadyRelayURL() string { return "https://relay.example" }
func (*testRelays) ObserveAvailability(observe func(relayset.SenderAvailability)) {
	observe(relayset.SenderAvailability{Available: 1, Total: 1, EverReady: true})
}
func (*testRelays) Wake() {}
func (r *testRelays) StopRecovery() {
	r.once.Do(func() { r.step("recovery_stopped"); close(r.stopped) })
}
func (r *testRelays) Cleanup(context.Context) error {
	r.step("registrations_released")
	return r.cleanupErr
}

type testSessionFactory struct {
	relays           *testRelays
	step             func(string)
	stopErr          error
	entered, release chan struct{}
	once             sync.Once
	observeStop      func()
}

func (*testSessionFactory) AdmitChannel(context.Context, protocolsession.FrameChannel) (sessionruntime.SenderChannelAdmission, error) {
	return sessionruntime.SenderChannelAdmission{}, sessionruntime.ErrRuntimeClosed
}
func (f *testSessionFactory) Stop(ctx context.Context, _ string) error {
	f.once.Do(func() {
		f.step("admission_frozen")
		if f.entered != nil {
			close(f.entered)
		}
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f.release != nil {
		<-f.release
	}
	if f.observeStop != nil {
		f.observeStop()
	}
	f.step("sessions_joined")
	f.relays.StopRecovery()
	return errors.Join(f.stopErr, f.relays.Cleanup(ctx))
}

func fixtureRun(f *shareFixture, ctx context.Context, request Request) <-chan task.Completion[Result] {
	result := make(chan task.Completion[Result], 1)
	go func() { result <- Run(ctx, request, f.dependencies) }()
	return result
}
func awaitResult(t *testing.T, result <-chan task.Completion[Result]) task.Completion[Result] {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(testWaitTimeout):
		t.Fatal("share did not settle")
		return task.Completion[Result]{}
	}
}

func TestShareActivationPublishesBeforePrefetchAndJoinsBeforeSourceRelease(t *testing.T) {
	f := newShareFixture()
	f.factory.entered, f.factory.release = make(chan struct{}), make(chan struct{})
	ctx, stop := context.WithCancelCause(context.Background())
	done := fixtureRun(f, ctx, Request{Diagnostics: true, TraceLifecycle: true})
	ready, err := f.dependencies.Controller.Ready(context.Background())
	if err != nil || ready.RelayEndpoint.DialURL == "" {
		t.Fatalf("ready=%+v error=%v", ready, err)
	}
	if slices.Contains(f.snapshot(), "prefetch") {
		t.Fatal("descendant work started before capability publication")
	}
	f.step("capability_published")
	f.dependencies.Controller.Acknowledge(nil)
	if err := f.dependencies.Controller.Activated(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(task.ErrShareStopped)
	<-f.factory.entered
	if slices.Contains(f.snapshot(), "source_closed") {
		t.Fatal("source released while terminal work was active")
	}
	close(f.factory.release)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil || result.Outcome != task.OutcomeStopped || !result.Value.Ready {
		t.Fatalf("result=%+v", result)
	}
	steps := f.snapshot()
	for _, ordered := range [][2]string{
		{"capability_published", "prefetch"}, {"admission_frozen", "sessions_joined"},
		{"sessions_joined", "registrations_released"}, {"registrations_released", "source_closed"},
		{"network_joined", "source_closed"},
	} {
		if slices.Index(steps, ordered[0]) >= slices.Index(steps, ordered[1]) {
			t.Fatalf("ordering %v in %v", ordered, steps)
		}
	}
}

func TestSharePublicationFailureSettlesWithoutStartingDescendants(t *testing.T) {
	for _, stage := range []PublicationStage{PublicationEncoding, PublicationOutput} {
		t.Run(stageName(stage), func(t *testing.T) {
			f := newShareFixture()
			done := fixtureRun(f, context.Background(), Request{})
			if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("publication failed")
			f.dependencies.Controller.Acknowledge(&PublicationError{Stage: stage, Cause: failure})
			f.dependencies.Controller.Acknowledge(nil)
			result := awaitResult(t, done)
			class := task.FailureLocal
			if stage == PublicationEncoding {
				class = task.FailureUsage
			}
			if result.Outcome != task.OutcomeFailed || result.FailureClass != class || !errors.Is(result.Err, failure) {
				t.Fatalf("result=%+v", result)
			}
			if slices.Contains(f.snapshot(), "prefetch") {
				t.Fatal("publication failure started descendant work")
			}
			if err := f.dependencies.Controller.Activated(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("activation error=%v", err)
			}
		})
	}
}
func stageName(stage PublicationStage) string {
	if stage == PublicationEncoding {
		return "encoding"
	}
	return "output"
}

func TestSharePreparationFailuresReturnCleanupAndResolveReadiness(t *testing.T) {
	failure := errors.New("phase failure")
	cleanup := errors.New("source close failure")
	cases := []struct {
		name      string
		configure func(*shareFixture)
		class     task.FailureClass
	}{
		{"prepare", func(f *shareFixture) {
			f.dependencies.Prepare = func(context.Context, liveshare.SenderConfig) (Prepared, error) { return f.prepared, failure }
		}, task.FailureUsage},
		{"missing_prepared", func(f *shareFixture) {
			f.dependencies.Prepare = func(context.Context, liveshare.SenderConfig) (Prepared, error) { return nil, nil }
		}, task.FailureLocal},
		{"authorize", func(f *shareFixture) { f.prepared.authorizeErr = failure }, task.FailureLocal},
		{"relays", func(f *shareFixture) {
			f.dependencies.Relays = func(context.Context, []string, relayset.SenderFactory) (Relays, error) { return f.relays, failure }
		}, task.FailureNetwork},
		{"missing_relays", func(f *shareFixture) {
			f.dependencies.Relays = func(context.Context, []string, relayset.SenderFactory) (Relays, error) { return nil, nil }
		}, task.FailureLocal},
		{"readiness", func(f *shareFixture) { f.relays.waitErr = failure }, task.FailureNetwork},
		{"peers", func(f *shareFixture) {
			f.dependencies.Peers = func(nativeconnectivity.SenderConfig) (*v2peer.Factory, error) { return nil, failure }
		}, task.FailureLocal},
		{"missing_peers", func(f *shareFixture) {
			f.dependencies.Peers = func(nativeconnectivity.SenderConfig) (*v2peer.Factory, error) { return nil, nil }
		}, task.FailureLocal},
		{"factory", func(f *shareFixture) { f.prepared.runtimeErr = failure }, task.FailureLocal},
		{"missing_factory", func(f *shareFixture) { f.prepared.factory = nil }, task.FailureLocal},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newShareFixture()
			f.prepared.closeErr = cleanup
			test.configure(f)
			result := Run(context.Background(), Request{}, f.dependencies)
			if result.Outcome != task.OutcomeFailed || result.FailureClass != test.class || result.Err == nil {
				t.Fatalf("result=%+v", result)
			}
			if test.name != "missing_prepared" && !errors.Is(result.CleanupError, cleanup) {
				t.Fatalf("cleanup failure lost: %v", result.CleanupError)
			}
			if _, err := f.dependencies.Controller.Ready(context.Background()); err == nil {
				t.Fatal("failed preparation retained pending readiness")
			}
		})
	}
}

func TestShareCallerCancellationKeepsIndependentStopAndSourceFailures(t *testing.T) {
	for _, failureOwner := range []string{"factory", "source", "none"} {
		t.Run(failureOwner, func(t *testing.T) {
			f := newShareFixture()
			failure := errors.New("cleanup failed")
			want := task.FailureNone
			if failureOwner == "factory" {
				f.factory.stopErr = errors.Join(context.Canceled, failure)
				want = task.FailureNetwork
			}
			if failureOwner == "source" {
				f.prepared.closeErr = failure
				want = task.FailureLocal
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := fixtureRun(f, ctx, Request{})
			if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.dependencies.Controller.Acknowledge(nil)
			if err := f.dependencies.Controller.Activated(context.Background()); err != nil {
				t.Fatal(err)
			}
			cancel()
			result := awaitResult(t, done)
			if result.FailureClass != want || (result.Err != nil) != (failureOwner != "none") {
				t.Fatalf("result=%+v", result)
			}
			if failureOwner == "none" {
				if result.Outcome != task.OutcomeCancelled || result.CleanupError != nil {
					t.Fatalf("result=%+v", result)
				}
			} else if result.Outcome != task.OutcomeFailed || !errors.Is(result.CleanupError, failure) || !errors.Is(result.Err, failure) {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestShareCancelWhileWaitingForRelayNeverActivates(t *testing.T) {
	f := newShareFixture()
	waiting := make(chan struct{})
	f.relays.wait = func(ctx context.Context) error { close(waiting); <-ctx.Done(); return context.Cause(ctx) }
	ctx, cancel := context.WithCancelCause(context.Background())
	done := fixtureRun(f, ctx, Request{})
	<-waiting
	cancel(task.ErrApplicationClosed)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil || result.Outcome != task.OutcomeCancelled {
		t.Fatalf("result=%+v", result)
	}
	if _, err := f.dependencies.Controller.Ready(context.Background()); !errors.Is(err, task.ErrApplicationClosed) {
		t.Fatalf("ready error=%v", err)
	}
	if slices.Contains(f.snapshot(), "prefetch") {
		t.Fatal("canceled share activated")
	}
}

func TestShareServeFailureRetainsPrimaryFailureWithoutMislabelingCleanup(t *testing.T) {
	f := newShareFixture()
	failure := errors.New("relay accept failed")
	f.relays.acceptErr = failure
	done := fixtureRun(f, context.Background(), Request{})
	if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.dependencies.Controller.Acknowledge(nil)
	result := awaitResult(t, done)
	if !errors.Is(result.Err, failure) || result.CleanupError != nil || result.FailureClass != task.FailureNetwork {
		t.Fatalf("result=%+v", result)
	}
}

func TestStopFactoryDeadlineDoesNotReleaseRuntimeOwnership(t *testing.T) {
	f := newShareFixture()
	f.factory.entered, f.factory.release = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- stopFactoryWithin(ctx, f.factory, stoppedMessage) }()
	<-f.factory.entered
	select {
	case <-done:
		t.Fatal("stop deadline abandoned runtime join")
	default:
	}
	close(f.factory.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop error=%v", err)
	}
}
