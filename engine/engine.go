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

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/engine/internal/task"
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
}

func (failure *UnreleasedResourcesError) Error() string {
	return fmt.Sprintf("engine shutdown retained catalog usage %+v and %d cached bytes", failure.CatalogUsage, failure.CacheBytes)
}

type managedTask interface {
	Stop(task.StopReason)
	Done() <-chan struct{}
}

// Engine owns aggregate capacity across native operations. Once Close begins,
// task admission stays closed even if a caller stops waiting for shutdown.
type Engine struct {
	mu            sync.Mutex
	config        Config
	random        *lockedReader
	identity      string
	sequence      uint64
	revisions     *revisioncapacity.ProcessOwner
	catalog       *catalog.BudgetAccount
	cache         *contentflow.ProcessCacheBudget
	tasks         map[task.ID]managedTask
	closing       bool
	closed        chan struct{}
	closeErr      error
	cleanupErrors []error
}

func New(config Config) (*Engine, error) {
	if config.Now == nil {
		config.Now = time.Now
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
	if config.RevisionCapacity.Limits == (revisioncapacity.CapacityLimits{}) {
		config.RevisionCapacity.Limits = revisioncapacity.DefaultProcessLimits()
	}
	if config.RevisionCapacity.RetryAfter == 0 {
		config.RevisionCapacity.RetryAfter = revisioncapacity.DefaultCapacityRetryAfter
	}
	if config.CatalogLimits == (catalog.BudgetLimits{}) {
		config.CatalogLimits = catalog.DefaultProcessBudgetLimits()
	}
	if config.CacheBytes == 0 {
		config.CacheBytes = contentflow.DefaultProcessSealedCacheBytes
	}
	catalogBudget, err := catalog.NewBudgetAccount("engine-process", config.CatalogLimits)
	if err != nil {
		return nil, err
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(config.CacheBytes)
	if err != nil {
		return nil, err
	}
	revisions, err := revisioncapacity.NewProcessOwner(config.RevisionCapacity)
	if err != nil {
		return nil, err
	}
	random := &lockedReader{reader: config.Random}
	var identity [applicationIdentityBytes]byte
	if _, err = io.ReadFull(random, identity[:]); err != nil {
		_ = revisions.Close()
		return nil, fmt.Errorf("create engine identity: %w", err)
	}
	return &Engine{
		config: config, random: random, identity: hex.EncodeToString(identity[:]),
		revisions: revisions, catalog: catalogBudget, cache: cacheBudget,
		tasks: make(map[task.ID]managedTask), closed: make(chan struct{}),
	}, nil
}

func start[T any](engine *Engine, ctx context.Context, run func(context.Context, task.Control) task.Completion[T]) (*Task[T], error) {
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
	current, err := task.Start(ctx, task.Config[T]{
		ID: id, Now: engine.config.Now, Random: engine.random,
		CleanupTimeout:      engine.config.CleanupTimeout,
		ObservationCapacity: engine.config.ObservationCapacity,
		Run:                 run,
		Completed: func(cleanupErr error) {
			engine.mu.Lock()
			delete(engine.tasks, id)
			if cleanupErr != nil {
				engine.cleanupErrors = append(engine.cleanupErrors, fmt.Errorf("task %s cleanup: %w", id, cleanupErr))
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
	capacityErr := engine.revisions.Close()
	var retainedErr error
	catalogUsage, cacheBytes := engine.catalog.Snapshot().Used, engine.cache.Used()
	if catalogUsage != (catalog.ResourceUsage{}) || cacheBytes != 0 {
		retainedErr = &UnreleasedResourcesError{CatalogUsage: catalogUsage, CacheBytes: cacheBytes}
	}
	engine.mu.Lock()
	engine.closeErr = errors.Join(append(engine.cleanupErrors, capacityErr, retainedErr)...)
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
