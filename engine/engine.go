package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/relaysetup"
)

const (
	DefaultCleanupTimeout                                 = 20 * time.Second
	DefaultObservationCapacity observationstream.Capacity = 256
	applicationIdentityBytes                              = 16
)

var ErrClosed = errors.New("engine is closed")

// UnreleasedResourcesError identifies authority still charged after all tasks
// have settled. It prevents an ownership defect from becoming a clean shutdown.
type UnreleasedResourcesError struct {
	CatalogUsage catalog.ResourceUsage
	CacheBytes   uint64
	cause        error
}

func (failure *UnreleasedResourcesError) Error() string {
	return fmt.Sprintf("engine shutdown retained catalog usage %+v and %d cached bytes", failure.CatalogUsage, failure.CacheBytes)
}

func (failure *UnreleasedResourcesError) Unwrap() error { return failure.cause }

type managedTask interface {
	Stop(task.StopReason)
	Done() <-chan struct{}
}

// Engine owns aggregate capacity across its native operations, independently of
// other Engines in the process. Once Close begins,
// task admission stays closed even if a caller stops waiting for shutdown.
type Engine struct {
	relaySetup      *relaysetup.Service
	networkState    *networkstate.Watcher
	network         *networkruntime.Runtime
	mu              sync.Mutex
	config          Config
	random          *lockedReader
	identity        string
	sequence        uint64
	revisions       *content.HandlePool
	catalog         *catalog.BudgetAccount
	cache           *contentflow.ProcessCacheBudget
	receiveCapacity *liveshare.ReceiveCapacity
	connectivity    *nativeconnectivity.Capacity
	tasks           map[task.ID]managedTask
	closing         bool
	closed          chan struct{}
	closeErr        error
	cleanupErrors   []error
}

func New(config Config) (*Engine, error) {
	if config.Clock == nil {
		config.Clock = senderrelay.SystemClock{}
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = DefaultCleanupTimeout
	}
	if config.ObservationCapacity == 0 {
		config.ObservationCapacity = DefaultObservationCapacity
	}
	if config.CleanupTimeout < 0 || config.ObservationCapacity < 0 {
		return nil, errors.New("engine cleanup timeout and observation capacity must be positive")
	}
	if config.RevisionCapacity.Limits == (content.CapacityLimits{}) {
		config.RevisionCapacity.Limits = content.DefaultProcessCapacityLimits()
	}
	if config.RevisionCapacity.RetryAfter == 0 {
		config.RevisionCapacity.RetryAfter = content.DefaultCapacityRetryAfter
	}
	if config.CatalogLimits == (catalog.BudgetLimits{}) {
		config.CatalogLimits = catalog.DefaultProcessBudgetLimits()
	}
	if config.CacheBytes == 0 {
		config.CacheBytes = contentflow.DefaultProcessBlockCacheBytes
	}
	networkState := networkstate.NewWatcher(networkstate.WatcherConfig{Now: config.Clock.Now})
	connectivity, err := nativeconnectivity.NewCapacity(nativepeer.AdmissionClock{Now: config.Clock.Now}, networkState)
	if err != nil {
		networkState.Close()
		return nil, err
	}
	catalogBudget, err := catalog.NewBudgetAccount("engine-process", config.CatalogLimits)
	if err != nil {
		return nil, err
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(config.CacheBytes)
	if err != nil {
		return nil, err
	}
	revisions, err := content.NewHandlePool(config.RevisionCapacity)
	if err != nil {
		return nil, err
	}
	receiveCapacity, err := liveshare.NewReceiveCapacity(config.ReceiveCapacity)
	if err != nil {
		_ = revisions.Close()
		return nil, err
	}
	random := &lockedReader{reader: config.Random}
	var identity [applicationIdentityBytes]byte
	if _, err = io.ReadFull(random, identity[:]); err != nil {
		_ = receiveCapacity.Close()
		_ = revisions.Close()
		return nil, fmt.Errorf("create engine identity: %w", err)
	}
	engineID := hex.EncodeToString(identity[:])
	engineDiagnostics := config.Diagnostics.Bind(diagnostics.Scope{EngineID: engineID})
	network, err := networkruntime.New(config.Network, config.Clock.Now, engineID, engineDiagnostics, networkState)
	if err != nil {
		networkState.Close()
		_ = receiveCapacity.Close()
		_ = revisions.Close()
		return nil, err
	}
	return &Engine{
		networkState: networkState,
		network:      network,
		relaySetup:   relaysetup.New(config.RelaySetup),
		config:       config, random: random, identity: engineID,
		revisions: revisions, catalog: catalogBudget, cache: cacheBudget, connectivity: connectivity,
		receiveCapacity: receiveCapacity,
		tasks:           make(map[task.ID]managedTask), closed: make(chan struct{}),
	}, nil
}

func start[T any](engine *Engine, ctx context.Context, run func(context.Context, task.Control) task.Completion[T]) (*Task[T], error) {
	return startWorkflow(engine, ctx, "", diagnostics.Sink{}, run)
}
func startWorkflow[T any](engine *Engine, ctx context.Context, workflow string, diagnosticSink diagnostics.Sink, run func(context.Context, task.Control) task.Completion[T]) (*Task[T], error) {
	if ctx == nil {
		return nil, errors.New("task context is nil")
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closing {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	engine.sequence++
	id := task.ID(fmt.Sprintf("%s:%d", engine.identity, engine.sequence))
	var project func(task.Observation, func(task.Observation) bool) bool
	if workflow != "" {
		project = func(value task.Observation, emit func(task.Observation) bool) bool {
			return translateWorkflowEvent(workflow, value, emit)
		}
	}
	current, err := task.Start(ctx, task.Config[T]{
		ID: id, Now: engine.config.Clock.Now, Random: engine.random,
		Diagnostics:         diagnosticSink.Bind(diagnostics.Scope{EngineID: engine.identity, TaskID: string(id), Workflow: workflow}),
		CleanupTimeout:      engine.config.CleanupTimeout,
		ObservationCapacity: engine.config.ObservationCapacity,
		Project:             project,
		Run: func(ctx context.Context, control task.Control) task.Completion[T] {
			result := run(ctx, control)
			result.Settlement = result.Checked()
			return result
		},
		Completed: func(localCleanupErr error) {
			engine.mu.Lock()
			delete(engine.tasks, id)
			if localCleanupErr != nil {
				engine.cleanupErrors = append(engine.cleanupErrors, fmt.Errorf("task %s cleanup: %w", id, localCleanupErr))
			}
			engine.mu.Unlock()
		},
	})
	if err != nil {
		return nil, err
	}
	engine.tasks[id] = current
	return &Task[T]{current: current}, nil
}

func (engine *Engine) Close(ctx context.Context) error {
	engine.mu.Lock()
	if !engine.closing {
		engine.closing = true
		active := make([]managedTask, 0, len(engine.tasks))
		for _, current := range engine.tasks {
			active = append(active, current)
		}
		go engine.shutdown(active)
	}
	engine.mu.Unlock()
	select {
	case <-engine.closed:
		return engine.shutdownError()
	default:
	}
	select {
	case <-engine.closed:
		return engine.shutdownError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (engine *Engine) shutdown(active []managedTask) {
	for _, current := range active {
		current.Stop(task.ApplicationClosed)
	}
	for _, current := range active {
		<-current.Done()
	}
	engine.network.Close()
	engine.networkState.Close()
	capacityErr := engine.revisions.Close()
	receiveCapacityErr := engine.receiveCapacity.Close()
	var retainedErr error
	catalogUsage, cacheBytes := engine.catalog.Snapshot().Used, engine.cache.Used()
	cacheErr := engine.cache.Close()
	if catalogUsage != (catalog.ResourceUsage{}) || cacheErr != nil {
		retainedErr = &UnreleasedResourcesError{CatalogUsage: catalogUsage, CacheBytes: cacheBytes, cause: cacheErr}
	}
	engine.mu.Lock()
	engine.closeErr = errors.Join(append(engine.cleanupErrors, capacityErr, receiveCapacityErr, retainedErr)...)
	engine.cleanupErrors = nil
	engine.mu.Unlock()
	close(engine.closed)
}

func (engine *Engine) shutdownError() error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.closeErr
}

type lockedReader struct {
	mu     sync.Mutex
	reader io.Reader
}

func (reader *lockedReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reader.Read(buffer)
}
