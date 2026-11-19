package desktop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
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
	Done() <-chan struct{}
	Observations() <-chan engine.Observation
	Cancel()
	CancelBeforeReady() bool
	StopShare()
}

type accountService interface {
	Snapshot() (account.Snapshot, <-chan struct{})
	StartRenewal()
	Retry()
	Register(context.Context) error
	Login(context.Context, string, string) error
	SetPassword(context.Context, string, string, string) error
	SetNickname(context.Context, string, string) error
	RequestProfileEmail(context.Context, string, string) error
	ConfirmProfileEmail(context.Context, string, string, string) error
	RequestEmail(context.Context, string) error
	VerifyEmail(context.Context, string) error
	RevokeCredential(context.Context, [16]byte) error
}

type dependencies struct {
	defaultFrontend string
	accounts        accountService
	start           func(context.Context, engine.ShareRequest) (shareTask, error)
	close           func(context.Context) error
	load            func() (relayconfig.Config, error)
	configure       func(turn.Settings) error
	setup           func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error)
	cancelSetup     func(context.Context, string) error
	log             func(string, any)
	diagnostics     diagnostics.Sink
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
type awaitingAuthorization struct{ authority engine.ShareAuthorization }
type admission struct{ cancelling bool }
type running struct {
	task           shareTask
	frontURL       string
	selectedRelays bool
}
type terminal struct {
	result Completion
	error  *Failure
}

func (awaitingConfiguration) operationPhase() {}
func (awaitingNetwork) operationPhase()       {}
func (awaitingAuthorization) operationPhase() {}
func (admission) operationPhase()             {}
func (running) operationPhase()               {}
func (terminal) operationPhase()              {}

type operation struct {
	id                string
	sourceOperationID string
	paths             []string
	requestedRelays   []string
	relayOverride     []string
	createdAt         time.Time
	phase             operationPhase
	cancel            context.CancelFunc
}

// Application owns intents independently of connections, navigation and startup.
// Only committed state is observed; slow admission and setup never hold mu.
type Application struct {
	mu              sync.Mutex
	sessionID       string
	snapshotValue   Snapshot
	operations      map[string]*operation
	deps            dependencies
	closing         bool
	admissions      chan struct{}
	work            sync.WaitGroup
	readers         sync.WaitGroup
	subscribers     map[*subscription]struct{}
	preferences     relayconfig.Preferences
	setup           SetupView
	setupRequests   map[string]setupCommand
	interruptSetup  context.CancelFunc
	accountContext  context.Context
	cancelAccount   context.CancelFunc
	accountView     *AccountView
	accountRequests map[string][32]byte
}

func newApplication(sessionID string, deps dependencies) *Application {
	a := &Application{
		accountRequests: make(map[string][32]byte),
		sessionID:       sessionID, operations: make(map[string]*operation), deps: deps,
		admissions: make(chan struct{}, 1), subscribers: make(map[*subscription]struct{}),
		setup:         SetupView{State: "idle"},
		setupRequests: make(map[string]setupCommand),
	}
	a.preferences = (relayconfig.Config{}).Preferences(deps.defaultFrontend)
	a.accountContext, a.cancelAccount = context.WithCancel(context.Background())
	a.watchAccount()
	return a
}

func (a *Application) execute(request Request) Message {
	if request.Command == "preferences" {
		settings, err := a.deps.load()
		a.mu.Lock()
		defer a.mu.Unlock()
		reply := Message{Version: ProtocolVersion, ID: request.ID}
		if err != nil {
			reply.Error = failure("settings_unavailable", err)
			return reply
		}
		a.preferences = settings.Preferences(a.deps.defaultFrontend)
		snapshot := a.snapshotLocked()
		reply.Snapshot = &snapshot
		return reply
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	switch request.Command {
	case "account":
		err = a.accountCommand(request)
	case "start_account_sync":
		err = a.startAccountSync()
	case "create":
		err = a.create(request)
	case "repeat":
		err = a.repeat(request)
	case "configure":
		err = a.configure(request)
	case "cancel_setup":
		err = a.cancelSetupRequest(request.SetupID)
	case "resume":
		err = a.resume(request.OperationID)
	case "switch_relay":
		err = a.switchRelay(request.OperationID, request.RelayURL)
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
	}
	if request.Command != "list" && request.Command != "query" {
		a.logRequestResult(request, reply.Error)
	}
	if err == nil {
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
	if a.setup.OperationID == id {
		a.cancelSetupLocked()
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
	a.cancelAccount()
	a.cancelSetupLocked()
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
	a.mu.Lock()
	if a.setup.State == "cancel_failed" {
		err = errors.Join(err, errors.New(a.setup.Error.Message))
	}
	a.mu.Unlock()
	a.readers.Wait()
	a.deps.log("host.engine_joined", map[string]string{"error": errorText(err)})
	return err
}
