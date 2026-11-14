package desktop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/engine/turn"
)

const (
	maximumOperations       = 1024
	maximumOperationIDBytes = 128
)

type shareTask interface {
	ID() engine.TaskID
	Snapshot() engine.ShareSnapshot
	Changes() <-chan struct{}
	Observations() <-chan engine.Observation
	Cancel()
	StopShare()
}

type dependencies struct {
	start         func(context.Context, engine.ShareRequest) (shareTask, error)
	close         func(context.Context) error
	load          func() (relayconfig.Config, error)
	configure     func(turn.Settings) error
	networkStatus func() *engine.NetworkUnavailableError
	setup         func(context.Context, SetupInput, func(relaysetup.Progress)) error
	log           func(string, any)
	diagnostics   diagnostics.Sink
}

type operationPhase interface{ operationPhase() }
type awaitingConfiguration struct {
	block   CreationBlock
	failure *Failure
}
type awaitingNetwork struct {
	block   CreationBlock
	failure *Failure
}
type admission struct{ cancelling bool }
type running struct {
	task     shareTask
	frontURL string
}
type terminal struct {
	result Completion
	error  *Failure
}

func (awaitingConfiguration) operationPhase() {}
func (awaitingNetwork) operationPhase()       {}
func (admission) operationPhase()             {}
func (running) operationPhase()               {}
func (terminal) operationPhase()              {}

type operation struct {
	id        string
	paths     []string
	relays    []string
	createdAt time.Time
	phase     operationPhase
	cancel    context.CancelFunc
}

// Application owns intents independently of connections, navigation and startup.
// Only committed state is observed; slow admission and setup never hold mu.
type Application struct {
	mu            sync.Mutex
	sessionID     string
	snapshotValue Snapshot
	operations    map[string]*operation
	deps          dependencies
	closing       bool
	admissions    chan struct{}
	work          sync.WaitGroup
	readers       sync.WaitGroup
	subscribers   map[*subscription]struct{}
	setup         SetupView
	setupRequests map[string]setupCommand
	cancelSetup   context.CancelFunc
}

func newApplication(sessionID string, deps dependencies) *Application {
	return &Application{
		sessionID: sessionID, operations: make(map[string]*operation), deps: deps,
		admissions: make(chan struct{}, 1), subscribers: make(map[*subscription]struct{}),
		setup:         SetupView{State: "idle"},
		setupRequests: make(map[string]setupCommand),
	}
}

func (a *Application) execute(request Request) Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if request.Command != "list" && request.Command != "query" {
		a.deps.log("host.request", request)
	}
	var err error
	switch request.Command {
	case "create":
		err = a.create(request)
	case "configure":
		err = a.configure(request)
	case "resume":
		err = a.resume(request.OperationID)
	case "cancel":
		err = a.cancel(request.OperationID)
	case "stop":
		err = a.stop(request.TaskID)
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

func (a *Application) stop(id string) error {
	for _, op := range a.operations {
		if current, ok := op.phase.(running); ok && string(current.task.ID()) == id {
			current.task.StopShare()
			return nil
		}
	}
	return errors.New("share task not found")
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
		// A cancellation can precede replay on another connection.
		op = &operation{id: id, createdAt: time.Now()}
		a.operations[id] = op
	}
	if op.cancel != nil {
		op.cancel()
	}
	switch current := op.phase.(type) {
	case running:
		current.task.Cancel()
	case admission:
		op.phase = admission{cancelling: true}
	case terminal:
	default:
		op.phase = terminal{result: Completion{Outcome: OutcomeCancelled, StopReason: Cancelled}}
	}
	return nil
}

func (a *Application) close() error {
	a.mu.Lock()
	a.closing = true
	if a.cancelSetup != nil {
		a.cancelSetup()
	}
	for _, op := range a.operations {
		if _, admitting := op.phase.(admission); admitting && op.cancel != nil {
			op.cancel()
		}
	}
	a.mu.Unlock()
	// Admission may be returning an engine handle. Join it before closing the
	// engine so no late worker can escape the application's ownership barrier.
	a.work.Wait()
	err := a.deps.close(context.Background())
	a.readers.Wait()
	a.deps.log("host.engine_joined", map[string]string{"error": errorText(err)})
	return err
}
