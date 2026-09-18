package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/engine/internal/task"
)

func newTestEngine(t *testing.T, config Config) *Engine {
	t.Helper()
	application, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return application
}

func TestApplicationCloseJoinsTasksWithIndependentCleanup(t *testing.T) {
	application := newTestEngine(t, Config{Now: func() time.Time { return time.Unix(123, 0) }})
	const count = 3
	started := make(chan struct{}, count)
	cleaning := make(chan struct{}, count)
	release := make(chan struct{})
	tasks := make([]*Task[int], 0, count)
	for i := range count {
		current, err := start(application, context.Background(), func(ctx context.Context, control task.Control) task.Completion[int] {
			started <- struct{}{}
			<-ctx.Done()
			cleanup, cancel := control.CleanupContext()
			defer cancel()
			if cleanup.Err() != nil {
				return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: cleanup.Err(), CleanupError: cleanup.Err()}}
			}
			cleaning <- struct{}{}
			<-release
			return task.Completion[int]{Value: i, Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, current)
	}
	for range count {
		<-started
	}
	wait, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Close(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("close wait = %v", err)
	}
	for range count {
		<-cleaning
	}
	if _, err := start(application, context.Background(), func(context.Context, task.Control) task.Completion[int] { return task.Completion[int]{} }); !errors.Is(err, ErrClosed) {
		t.Fatalf("admission after close = %v", err)
	}
	for _, current := range tasks {
		if current.State() != TaskStopping {
			t.Fatalf("state = %v", current.State())
		}
		select {
		case <-current.Done():
			t.Fatal("task completed while cleanup held")
		default:
		}
	}
	close(release)
	if err := application.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	identities := make(map[TaskID]bool)
	for i, current := range tasks {
		result, err := current.Wait(context.Background())
		if err != nil || result.Value != i || result.StopReason != ApplicationClosed ||
			!result.StartedAt.Equal(time.Unix(123, 0)) || !result.FinishedAt.Equal(result.StartedAt) {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if identities[current.ID()] {
			t.Fatal("tasks share an operation identity")
		}
		identities[current.ID()] = true
		for range current.Observations() {
		}
	}
	application.mu.Lock()
	defer application.mu.Unlock()
	if len(application.tasks) != 0 {
		t.Fatal("finished tasks remain retained")
	}
}

func TestFinalCleanupFailureSurvivesTaskAndApplicationShutdown(t *testing.T) {
	cleanupErr := errors.New("source handle shutdown failed")
	application, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	current, err := start(application, context.Background(), func(context.Context, task.Control) task.Completion[int] {
		return task.Completion[int]{Value: 9, Settlement: task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: cleanupErr, CleanupError: cleanupErr}}
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(context.Background())
	if err != nil || result.Value != 9 || !errors.Is(result.CleanupError, cleanupErr) {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if err := application.Close(context.Background()); !errors.Is(err, cleanupErr) || !strings.Contains(err.Error(), string(current.ID())) {
		t.Fatalf("application cleanup = %v", err)
	}
	if err := application.Close(context.Background()); !errors.Is(err, cleanupErr) {
		t.Fatalf("repeated close = %v", err)
	}
}

func TestNewValidatesDependenciesAndDoesNotShareOwners(t *testing.T) {
	for _, config := range []Config{
		{CleanupTimeout: -time.Second},
		{ObservationCapacity: -1},
		{CatalogLimits: catalog.BudgetLimits{MemoryBytes: 1}},
		{RevisionCapacity: revisioncapacity.ProcessConfig{Limits: revisioncapacity.CapacityLimits{StableHandles: 1}}},
		{Random: bytes.NewReader(nil)},
	} {
		if application, err := New(config); err == nil {
			_ = application.Close(context.Background())
			t.Fatal("invalid config accepted")
		}
	}
	first := newTestEngine(t, Config{})
	second := newTestEngine(t, Config{})
	if first.revisions.Coordinator() == second.revisions.Coordinator() || first.catalog == second.catalog || first.cache == second.cache {
		t.Fatal("independent applications share an aggregate owner")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := func(context.Context, task.Control) task.Completion[int] { return task.Completion[int]{} }
	if _, err := start(first, ctx, run); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled start = %v", err)
	}
	if _, err := start(first, nil, run); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestInjectedEntropyIsSerializedAcrossTasks(t *testing.T) {
	entropy := &exclusiveReader{}
	application := newTestEngine(t, Config{Random: entropy})
	var group sync.WaitGroup
	const count = 20
	for range count {
		group.Go(func() {
			current, err := start(application, context.Background(), func(_ context.Context, control task.Control) task.Completion[int] {
				var buffer [32]byte
				_, err := io.ReadFull(control.Random, buffer[:])
				if err == nil {
					return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
				}
				return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: err}}
			})
			if err != nil {
				t.Error(err)
				return
			}
			result, err := current.Wait(context.Background())
			if err != nil || result.Err != nil {
				t.Errorf("entropy = %v, %v", result.Err, err)
			}
		})
	}
	group.Wait()
}

type exclusiveReader struct{ mu sync.Mutex }

func (reader *exclusiveReader) Read(buffer []byte) (int, error) {
	if !reader.mu.TryLock() {
		return 0, errors.New("injected entropy accessed concurrently")
	}
	defer reader.mu.Unlock()
	for i := range buffer {
		buffer[i] = byte(i + 1)
	}
	return len(buffer), nil
}

func TestReceiveFacadeSettlesInvalidRequestAndClosesAdmission(t *testing.T) {
	application := newTestEngine(t, Config{})
	current, err := application.StartReceive(context.Background(), ReceiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(context.Background())
	failure, classified := errors.AsType[ReceiveFailure](result.Err)
	if err != nil || result.Outcome != OutcomeFailed || result.FailureClass != FailureUsage || !classified || failure.Code != ReceiveFailureInvalidInput || !result.Settlement.Valid() {
		t.Fatalf("result = %+v, %v", result, err)
	}
	finished := 0
	for observation := range current.Observations() {
		if lifecycle, ok := observation.Event.(LifecycleObservation); ok && lifecycle.State == TaskFinished {
			finished++
			if lifecycle.Settlement != result.Settlement || lifecycle.Err == nil {
				t.Fatalf("failed receive lost its terminal reason: %+v", lifecycle)
			}
		}
	}
	if finished != 1 {
		t.Fatalf("finished observations = %d", finished)
	}
	if err := application.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := application.StartReceive(context.Background(), ReceiveRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed receive = %v", err)
	}
	if _, err := application.StartShare(context.Background(), ShareRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed share = %v", err)
	}
}
