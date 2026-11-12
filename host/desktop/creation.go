package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
)

func (a *Application) create(request Request) error {
	if a.closing {
		return engine.ErrClosed
	}
	if !validOperation(request.OperationID) {
		return errors.New("operation_id must contain 1–128 bytes")
	}
	paths := make([]string, len(request.Paths))
	for i, path := range request.Paths {
		if path == "" {
			return errors.New("selection contains an empty path")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		paths[i] = absolute
	}
	if previous, ok := a.operations[request.OperationID]; ok {
		if previous.paths != nil && !slices.Equal(previous.paths, paths) {
			return errors.New("operation_id already belongs to another selection")
		}
		return nil
	}
	if len(paths) == 0 {
		return errors.New("select at least one file or folder")
	}
	if len(a.operations) >= maximumOperations {
		return errors.New("application operation limit reached; restart after stopping shares")
	}
	op := &operation{id: request.OperationID, paths: paths, createdAt: time.Now()}
	a.operations[op.id] = op
	a.admit(op)
	return nil
}

func (a *Application) resume(id string) error {
	if a.closing {
		return engine.ErrClosed
	}
	op, ok := a.operations[id]
	if !ok {
		return errors.New("creation operation not found")
	}
	if _, waiting := op.phase.(awaitingConfiguration); waiting {
		a.admit(op)
	}
	// Replayed resume never retries a terminal operation.
	return nil
}

func (a *Application) admit(op *operation) {
	ctx, cancel := context.WithCancel(context.Background())
	op.phase, op.cancel = admission{}, cancel
	a.work.Go(func() {
		a.runAdmission(ctx, op)
	})
}

func (a *Application) runAdmission(ctx context.Context, op *operation) {
	// TURN configuration is engine-wide. Serialize admission without blocking
	// control requests, and allow queued selections to be cancelled immediately.
	select {
	case a.admissions <- struct{}{}:
		defer func() { <-a.admissions }()
	case <-ctx.Done():
		a.completeAdmission(ctx, op, terminal{})
		return
	}
	a.completeAdmission(ctx, op, a.prepareAdmission(ctx, op))
}

func (a *Application) prepareAdmission(ctx context.Context, op *operation) operationPhase {
	config, err := a.deps.load()
	if err != nil {
		return awaitingConfiguration{failure: failure("settings_unavailable", err)}
	}
	relays, err := relayconfig.SelectRelays(config, op.relays)
	if err == nil {
		err = relayconfig.ValidateFrontURL(config.FrontURL)
	}
	if err != nil {
		return awaitingConfiguration{failure: failure("configuration_required", err)}
	}
	if err := a.deps.configure(config.TURNSettings()); err != nil {
		return awaitingConfiguration{failure: failure("settings_unavailable", err)}
	}
	if ctx.Err() != nil {
		return terminal{}
	}
	task, err := a.deps.start(ctx, engine.ShareRequest{
		Source: filesystem.ShareSource(op.paths), Relays: relays, Diagnostics: a.deps.diagnostics,
	})
	if task != nil {
		return running{task: task, frontURL: config.FrontURL}
	}
	if err == nil {
		err = errors.New("engine returned no share task")
	}
	return terminal{result: Completion{Outcome: OutcomeFailed, StopReason: NoStopReason, Error: err.Error()}, error: failure("creation_failed", err)}
}

func (a *Application) completeAdmission(ctx context.Context, op *operation, phase operationPhase) {
	a.mu.Lock()
	defer a.mu.Unlock()
	op.phase = phase
	if current, ok := phase.(running); ok {
		if a.closing || ctx.Err() != nil {
			current.task.Cancel()
		}
		a.deps.log("host.share_created", map[string]string{"operation_id": op.id, "task_id": string(current.task.ID())})
		a.readers.Add(1)
		go a.observe(op.id, current.task)
	} else {
		if a.closing || ctx.Err() != nil {
			op.phase = terminal{result: Completion{Outcome: OutcomeCancelled, StopReason: Cancelled}}
		}
		op.cancel()
		op.cancel = nil
	}
	a.snapshotLocked()
}
