package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
)

const (
	maximumOperations       = 1024
	maximumOperationIDBytes = 128
)

type shareTask interface {
	ID() engine.TaskID
	Snapshot() engine.ShareSnapshot
	Observations() <-chan engine.Observation
	Cancel()
	StopShare()
}

type dependencies struct {
	start       func(engine.ShareRequest) (shareTask, error)
	close       func(context.Context) error
	load        func() (relayconfig.Config, error)
	configure   func(turn.Settings) error
	log         func(string, any)
	diagnostics diagnostics.Sink
}

type operation struct {
	id          string
	paths       []string
	frontURL    string
	task        shareTask
	err         *Failure
	cancelled   bool
	lastState   string
	lastStartup string
}

// Application retains commands and task handles until joined shutdown. The
// request connection is never an owner of a task or of engine cancellation.
type Application struct {
	mu          sync.Mutex
	sessionID   string
	revision    uint64
	operations  map[string]*operation
	deps        dependencies
	closing     bool
	readers     sync.WaitGroup
	subscribers map[chan Message]struct{}
}

func newApplication(sessionID string, deps dependencies) *Application {
	return &Application{sessionID: sessionID, operations: make(map[string]*operation), deps: deps, subscribers: make(map[chan Message]struct{})}
}

func (a *Application) execute(request Request) Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deps.log("host.request", request)
	var err error
	switch request.Command {
	case "create":
		err = a.create(request)
	case "cancel":
		err = a.cancel(request.OperationID)
	case "stop":
		var found bool
		for _, op := range a.operations {
			if op.task != nil && string(op.task.ID()) == request.TaskID {
				op.task.StopShare()
				found = true
				break
			}
		}
		if !found {
			err = errors.New("share task not found")
		}
	case "list", "observe":
	case "query":
		if _, ok := a.operations[request.OperationID]; !ok {
			err = errors.New("creation operation not found")
		}
	default:
		err = errors.New("unknown host command")
	}
	reply := Message{Version: ProtocolVersion, ID: request.ID}
	if err != nil {
		reply.Error = failure("request_rejected", err)
	} else {
		snapshot := a.snapshotLocked()
		reply.Snapshot = &snapshot
	}
	return reply
}

func validOperation(id string) bool { return len(id) > 0 && len(id) <= maximumOperationIDBytes }

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
	op := &operation{id: request.OperationID, paths: paths}
	a.operations[op.id] = op
	config, err := a.deps.load()
	if err != nil {
		op.err = failure("configuration_required", fmt.Errorf("read saved WindShare configuration: %w", err))
		return nil
	}
	relays, err := relayconfig.SelectRelays(config, nil)
	if err != nil || config.FrontURL == "" {
		op.err = failure("configuration_required", errors.New("save an active relay and frontend with 'wind relay add <url>' or 'wind config import <file> --front-url <url>', then create a new share"))
		return nil
	}
	if err := a.deps.configure(config.TURNSettings()); err != nil {
		op.err = failure("configuration_required", err)
		return nil
	}
	op.frontURL = config.FrontURL
	op.task, err = a.deps.start(engine.ShareRequest{Source: filesystem.ShareSource(paths), Relays: relays, Diagnostics: a.deps.diagnostics})
	if err != nil {
		op.err = failure("creation_failed", err)
		return nil
	}
	a.deps.log("host.share_created", map[string]string{"operation_id": op.id, "task_id": string(op.task.ID())})
	a.readers.Add(1)
	go a.observe(op.id, op.task)
	return nil
}

func (a *Application) cancel(id string) error {
	if !validOperation(id) {
		return errors.New("operation_id is required")
	}
	op, ok := a.operations[id]
	if !ok {
		if a.closing {
			return engine.ErrClosed
		}
		if len(a.operations) >= maximumOperations {
			return errors.New("application operation limit reached")
		}
		// A cancellation may arrive before a retried create on another connection.
		// Retaining this tombstone prevents a late request from resurrecting it.
		op = &operation{id: id}
		a.operations[id] = op
	}
	op.cancelled = true
	if op.task != nil {
		op.task.Cancel()
	}
	return nil
}

func (a *Application) close() error {
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
	err := a.deps.close(context.Background())
	a.readers.Wait()
	a.deps.log("host.engine_joined", map[string]string{"error": errorText(err)})
	return err
}
