package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
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
	op, err := a.registerOperation(request.OperationID, paths, request.Relays, "")
	if err == nil && op != nil {
		a.admit(op)
	}
	return err
}

func (a *Application) registerOperation(id string, paths, relays []string, sourceID string) (*operation, error) {
	if a.closing {
		return nil, engine.ErrClosed
	}
	if !validOperation(id) {
		return nil, errors.New("operation_id must contain 1–128 bytes")
	}
	if previous, ok := a.operations[id]; ok {
		if previous.paths != nil && (!slices.Equal(previous.paths, paths) || !slices.Equal(previous.requestedRelays, relays) || previous.sourceOperationID != sourceID) {
			return nil, errors.New("operation_id already belongs to another selection")
		}
		return nil, nil
	}
	if len(paths) == 0 {
		return nil, errors.New("select at least one file or folder")
	}
	if len(a.operations) >= maximumOperations {
		return nil, errors.New("application operation limit reached; restart after stopping shares")
	}
	relays = slices.Clone(relays)
	op := &operation{id: id, paths: slices.Clone(paths), requestedRelays: relays, explicitRelays: relays, sourceOperationID: sourceID, createdAt: time.Now()}
	a.operations[op.id] = op
	return op, nil
}

type resumeAction uint8

const (
	resumeWithCurrentSelection resumeAction = iota
	resumeWithSavedSettings
)

func (a *Application) resume(id string, action resumeAction) error {
	if a.closing {
		return engine.ErrClosed
	}
	op, ok := a.operations[id]
	if !ok {
		return errors.New("creation operation not found")
	}
	// A replay can arrive after the share has started or been cancelled.
	if !waitingForAdmission(op.phase) || (a.setup.OperationID == id && a.setup.busy()) {
		return nil
	}
	if action == resumeWithSavedSettings {
		op.explicitRelays = nil
		a.deps.log("host.share_using_saved_settings", map[string]string{"operation_id": id})
	}
	a.admit(op)
	return nil
}

func (a *Application) admit(op *operation) {
	a.admitAfter(op, nil)
}

func (a *Application) admitAfter(op *operation, previous <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	op.phase, op.cancel = admission{}, cancel
	// The admission intent fixes the capture interval. A queued or retried
	// connection cannot join a later recording by keeping an old sink.
	trace := a.deps.senderTrace.captureSink()
	a.work.Go(func() {
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
				a.completeAdmission(ctx, op, terminal{})
				return
			}
		}
		a.runAdmission(ctx, op, trace)
	})
}

func (a *Application) runAdmission(ctx context.Context, op *operation, trace diagnostics.Sink) {
	if ctx.Err() != nil {
		a.completeAdmission(ctx, op, terminal{})
		return
	}
	a.completeAdmission(ctx, op, a.prepareAdmission(ctx, op, trace))
}

var errFrontendRequired = errors.New("configure a receiver website before sharing")

func (a *Application) prepareAdmission(ctx context.Context, op *operation, trace diagnostics.Sink) operationPhase {
	config, err := a.deps.load()
	if err != nil {
		return configurationBlocked(blockingSettingsUnavailable, err)
	}
	intent, err := relayconfig.ShareIntent(config, op.explicitRelays)
	if err != nil {
		return configurationBlocked(blockingConfigurationRequired, err)
	}
	if blocked := a.validateFrontendAdmission(op.id, config.FrontURL, intent); blocked != nil {
		return blocked
	}
	if ctx.Err() != nil {
		return terminal{}
	}
	task, err := a.deps.start(ctx, engine.ShareRequest{
		Source: filesystem.ShareSource(op.paths), RelayIntent: intent, TURNServices: config.TURNServices, Frontend: config.FrontURL, Diagnostics: trace,
	})
	if task != nil {
		return running{task: task, frontURL: config.FrontURL, selectedRelays: intent.Mode() != relayprofile.Planned}
	}
	if err == nil {
		err = errors.New("engine returned no share task")
	}
	if errors.Is(err, engine.ErrNetworkUnavailable) {
		return networkBlocked(err)
	}
	return terminal{result: Completion{Outcome: OutcomeFailed, StopReason: NoStopReason, Error: errorText(err)}, error: engineFailure("creation_failed", err)}
}

func (a *Application) validateFrontendAdmission(operationID, explicitFrontend string, intent relayprofile.Intent) operationPhase {
	frontend := explicitFrontend
	if frontend == "" {
		frontend = a.deps.defaultFrontend
	}
	if frontend == "" {
		// On a fresh unprovisioned build without relays or a built-in frontend,
		// surface relay/network setup first so adding a relay can also expose
		// its advertised receiver website before prompting for one.
		if intent.Mode() == relayprofile.Planned && len(intent.Candidates()) == 0 && a.deps.networkUnavailability != nil {
			if unavailable := a.deps.networkUnavailability(); unavailable != nil {
				return networkBlocked(unavailable)
			}
		}
		a.deps.log("host.share_frontend_required", map[string]string{"operation_id": operationID})
		return configurationBlocked(blockingFrontendRequired, errFrontendRequired)
	}
	if err := relayconfig.ValidateFrontURL(frontend); err != nil {
		return configurationBlocked(blockingFrontendRequired, err)
	}
	return nil
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
