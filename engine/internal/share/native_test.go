package share

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/engine/internal/task"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type readyEndpoint struct {
	*testRelays
	completed atomic.Bool
}

func (*readyEndpoint) SetAvailabilityObserver(observe func(bool)) { observe(true) }
func (e *readyEndpoint) CompleteObservations() relayv2.LifecycleObservationCompletion {
	e.completed.Store(true)
	return relayv2.LifecycleObservationCompletion{}
}

func nativeDependencies(t *testing.T) Dependencies {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{Limits: revisioncapacity.DefaultProcessLimits(), RetryAfter: revisioncapacity.DefaultCapacityRetryAfter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := catalog.NewBudgetAccount("share-native-test", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	cache, err := contentflow.NewProcessCacheBudget(contentflow.DefaultProcessSealedCacheBytes)
	if err != nil {
		t.Fatal(err)
	}
	return Dependencies{
		Control: task.Control{ID: "native-share", Now: time.Now, Random: rand.Reader, CleanupContext: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), testWaitTimeout)
		}},
		Controller: NewController(), RevisionCapacity: owner.Coordinator(), CatalogBudget: budget, CacheBudget: cache,
	}
}

func nativeSource(paths []string) liveshare.FileSourceFactory {
	return liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{Paths: paths, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity)})
	})
}

func TestNativeShareUsesInjectedSourceAndCompletesRelayProducersBeforeFinalResult(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "selected.txt"), []byte("native content"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := nativeDependencies(t)
	var mu sync.Mutex
	var facts []Observation
	dependencies.Control.Emit = func(event task.Event) bool {
		if fact, ok := event.(Observation); ok {
			mu.Lock()
			facts = append(facts, fact)
			mu.Unlock()
		}
		return true
	}
	endpoint := &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}}
	dependencies.Relay = func(config senderrelay.Config) (Relay, error) {
		if config.Fresh.ShareID == (v2.ShareID{}) || len(config.Descriptor) == 0 {
			t.Error("registration authority missing")
		}
		config.ObserveAttempt(senderrelay.Attempt{ShareInstance: config.Fresh.ShareInstance, State: senderrelay.AttemptSucceeded, Number: 1, Generation: 1})
		config.ObserveConnection(senderrelay.Connection{})()
		return endpoint, nil
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan task.Completion[Result], 1)
	go func() {
		done <- Run(ctx, Request{Source: nativeSource([]string{root}), RelayURLs: []string{"https://relay.example"}, Diagnostics: true, TraceLifecycle: true}, dependencies)
	}()
	ready, err := dependencies.Controller.Ready(context.Background())
	if err != nil || ready.SelectedRootSummary.SelectedCount() != 1 {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	mu.Lock()
	acquired, prefetch := false, false
	for _, fact := range facts {
		acquired = acquired || fact.Milestone == SourceAcquired && !fact.ShareInstance.IsZero()
		prefetch = prefetch || fact.Prefetch != nil
	}
	mu.Unlock()
	if !acquired || prefetch {
		t.Fatalf("acquired=%v prefetchBeforePublication=%v", acquired, prefetch)
	}
	dependencies.Controller.Acknowledge(nil)
	if err := dependencies.Controller.Activated(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel(task.ErrShareStopped)
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil || !endpoint.completed.Load() {
		t.Fatalf("result=%+v endpointCompleted=%v", result, endpoint.completed.Load())
	}
}

func TestNativeSourceAcquisitionFailureKeepsCorrelatedTypedEvidence(t *testing.T) {
	dependencies := nativeDependencies(t)
	failure := errors.New("permission revoked")
	var acquired catalog.ShareInstance
	var facts []Observation
	dependencies.Control.Emit = func(event task.Event) bool {
		if fact, ok := event.(Observation); ok {
			facts = append(facts, fact)
		}
		return true
	}
	source := liveshare.FileSourceFactoryFunc(func(_ context.Context, value liveshare.FileSourceContext) (liveshare.FileSource, error) {
		acquired = value.ShareInstance
		return nil, failure
	})
	result := Run(context.Background(), Request{Source: source, RelayURLs: []string{"https://relay.example"}}, dependencies)
	if !errors.Is(result.Err, failure) || result.CleanupError != nil || acquired.IsZero() {
		t.Fatalf("result=%+v", result)
	}
	found := false
	for _, fact := range facts {
		if fact.Milestone == SourceAcquisitionFailed && fact.ShareInstance == acquired && errors.Is(fact.Failure, failure) {
			found = true
		}
	}
	if !found {
		t.Fatal("source acquisition failure lost share correlation")
	}
}

func TestRelayAssemblyRetainsNativeAuthorityAndRejectsInvalidMaterial(t *testing.T) {
	dependencies := nativeDependencies(t).normalized()
	o := newObservations(dependencies.Control, Request{})
	prepared, err := dependencies.Prepare(context.Background(), liveshare.SenderConfig{
		Source: nativeSource([]string{t.TempDir()}), Relays: []string{"https://relay.example"},
		Random: rand.Reader, Now: time.Now, RevisionCapacity: dependencies.RevisionCapacity,
		CatalogBudget: dependencies.CatalogBudget, CacheBudget: dependencies.CacheBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := prepared.AuthorizeRegistration(); err != nil {
		t.Fatal(err)
	}
	relay, err := prepareRelay(prepared, "https://relay.example", dependencies, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.complete()

	for _, invalid := range []liveshare.RegistrationMaterial{
		{}, {ShareID: make([]byte, 16)}, {ShareID: make([]byte, 16), ShareInstance: make([]byte, 16)},
	} {
		if _, _, _, err := relayRegistrationIdentity(invalid); err == nil {
			t.Fatal("invalid relay identity accepted")
		}
	}
	dependencies.Control.Random = exhaustedRandom{}
	if _, err := prepareRelay(prepared, "https://relay.example", dependencies, o); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("entropy error=%v", err)
	}
	dependencies.Control.Random = rand.Reader
	if _, err := prepareRelay(prepared, "invalid-endpoint", dependencies, o); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

type exhaustedRandom struct{}

func (exhaustedRandom) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadinessWaitCancellationDoesNotAcknowledgeOrCancelShare(t *testing.T) {
	controller := NewController()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.Ready(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("ready err=%v", err)
	}
	if err := controller.Activated(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("activated err=%v", err)
	}
	controller.publish(Ready{})
	entered := false
	controller.Acknowledge(nil)
	if err := controller.activate(context.Background(), func() { entered = true }); err != nil || !entered {
		t.Fatalf("activate err=%v entered=%v", err, entered)
	}
}

func TestActivationCancellationNeverStartsDescendantWork(t *testing.T) {
	controller := NewController()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.activate(canceled, func() { t.Error("canceled activation prefetched") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	controller.Acknowledge(nil)
	if err := controller.activate(canceled, func() { t.Error("canceled activation prefetched") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
