package share

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/internal/task"
)

type delayedCleanupRelays struct {
	*testRelays
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (r *delayedCleanupRelays) Cleanup(ctx context.Context) error {
	r.once.Do(func() { go func() { <-r.release; r.step("cleanup_joined"); close(r.done) }() })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return nil
	}
}

type boundedCleanupFactory struct {
	*testSessionFactory
	relays   *delayedCleanupRelays
	returned chan struct{}
}

func (f *boundedCleanupFactory) Stop(context.Context, string) error {
	f.step("admission_frozen")
	f.relays.StopRecovery()
	expired, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	err := f.relays.Cleanup(expired)
	close(f.returned)
	return err
}

func TestShareJoinsRelayCleanupEvenAfterFactoryTerminalWorkerReturned(t *testing.T) {
	f := newShareFixture()
	relays := &delayedCleanupRelays{testRelays: f.relays, release: make(chan struct{}), done: make(chan struct{})}
	factory := &boundedCleanupFactory{testSessionFactory: f.factory, relays: relays, returned: make(chan struct{})}
	f.prepared.factory = factory
	f.dependencies.Relays = func(context.Context, []string, relayset.SenderFactory) (Relays, error) { return relays, nil }
	ctx, stop := context.WithCancelCause(context.Background())
	done := fixtureRun(f, ctx, Request{})
	if _, err := f.dependencies.Controller.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.dependencies.Controller.Acknowledge(nil)
	if err := f.dependencies.Controller.Activated(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(task.ErrShareStopped)
	<-factory.returned
	if slices.Contains(f.snapshot(), "source_closed") {
		t.Fatal("factory terminal result released source before relay cleanup join")
	}
	select {
	case <-done:
		t.Fatal("share finished with relay cleanup still alive")
	default:
	}
	close(relays.release)
	result := awaitResult(t, done)
	if !errors.Is(result.CleanupError, context.DeadlineExceeded) || result.FailureClass != task.FailureNetwork {
		t.Fatalf("bounded cleanup failure lost: %+v", result)
	}
	steps := f.snapshot()
	if slices.Index(steps, "cleanup_joined") >= slices.Index(steps, "source_closed") {
		t.Fatalf("source teardown order=%v", steps)
	}
}
