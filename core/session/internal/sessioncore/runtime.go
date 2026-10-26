package sessioncore

import (
	"context"
	"fmt"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/observationstream"
	sessioncontract "github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"io"
	"sync"
	"time"
)

type LockedReader struct {
	mu     sync.Mutex
	reader io.Reader
}

func (reader *LockedReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reader.Read(destination)
}

type Runtime struct {
	peerPathControls     sessionControlHandler
	turnControls         sessionControlHandler
	share                catalog.ShareInstance
	role                 protocolsession.Role
	sessionID            protocolsession.ProtocolSessionID
	initial              sessioncontract.LaneIdentity
	keys                 protocolsession.SessionKeys
	random               io.Reader
	operations           *protocolsession.OperationTable
	router               *protocolsession.RoleRouter
	lanes                *runtimeLanes
	routes               *operationLaneRoutes
	now                  func() time.Time
	protocolObservations observationstream.Producer[sessionobs.ProtocolObservation]
	terminationObserver  TerminationObserver
	termination          runtimeTerminationArbiter

	ctx             context.Context
	cancelLifecycle context.CancelFunc
	done            chan struct{}
	work            sync.WaitGroup

	errMu      sync.Mutex
	err        error
	finishOnce sync.Once
	finalizeMu sync.Mutex
	finalizers []func()
	finalizing bool

	externalMu         sync.Mutex
	externalClosing    bool
	externalAdmissions sync.WaitGroup
}

type Config struct {
	InitialRequestTiming *requestlane.Lane
	Share                catalog.ShareInstance
	Role                 protocolsession.Role
	Keys                 protocolsession.SessionKeys
	LaneID               uint32
	LaneEpoch            uint32
	Channel              protocolsession.FrameChannel
	Random               io.Reader
	Authenticator        protocolsession.InboundMessageAuthenticator
	Continuations        protocolsession.OperationContinuationClassifier
	OperationLimits      protocolsession.OperationLimits
	RouterLimits         protocolsession.RouterLimits
	Now                  func() time.Time
	ProtocolObservations observationstream.Producer[sessionobs.ProtocolObservation]
	TerminationObserver  TerminationObserver
}

func New(config Config) (*Runtime, error) {
	if config.Share.IsZero() || config.Keys.ProtocolSessionID().IsZero() || config.LaneID == 0 ||
		config.Channel == nil || config.Random == nil || config.Authenticator == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	if config.OperationLimits == (protocolsession.OperationLimits{}) {
		config.OperationLimits = protocolsession.OperationLimits{
			MaxActive: sessioncontract.DefaultActiveOperations, MaxTracked: sessioncontract.DefaultTrackedOperations,
		}
	}
	if config.RouterLimits == (protocolsession.RouterLimits{}) {
		config.RouterLimits = protocolsession.DefaultRouterLimits
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	operations, err := protocolsession.NewOperationTableWithContinuations(
		config.OperationLimits, config.Now, config.Continuations,
	)
	if err != nil {
		return nil, err
	}
	router, err := protocolsession.NewRoleRouterWithLimits(config.Role, operations, config.RouterLimits)
	if err != nil {
		return nil, err
	}
	ctx, cancelLifecycle := context.WithCancel(context.Background())
	runtime := &Runtime{
		share: config.Share, role: config.Role, sessionID: config.Keys.ProtocolSessionID(),
		initial: sessioncontract.LaneIdentity{ID: config.LaneID, Epoch: config.LaneEpoch}, keys: config.Keys,
		random: config.Random, operations: operations, router: router,
		routes:               newOperationLaneRoutes(),
		now:                  config.Now,
		protocolObservations: config.ProtocolObservations,
		terminationObserver:  config.TerminationObserver,
		ctx:                  ctx,
		cancelLifecycle:      cancelLifecycle,
		done:                 make(chan struct{}),
	}
	for kind, handler := range map[protocolsession.MessageKind]*sessionControlHandler{
		protocolsession.MessagePeerPathControl: &runtime.peerPathControls,
		protocolsession.MessageTURNControl:     &runtime.turnControls,
	} {
		handler.runtime = runtime
		if err := router.RegisterHandler(kind, handler); err != nil {
			cancelLifecycle()
			return nil, err
		}
	}
	runtime.lanes = newRuntimeLanes(runtime)
	if _, err := runtime.lanes.add(runtime.initial, config.Channel, config.Authenticator, true); err != nil {
		cancelLifecycle()
		config.Keys.Destroy()
		return nil, err
	}
	if config.InitialRequestTiming != nil {
		runtime.lanes.active[runtime.initial.ID].requests = config.InitialRequestTiming
	}
	return runtime, nil
}

func trafficKey(keys protocolsession.SessionKeys, direction protocolsession.Direction) protocolsession.TrafficKey {
	if direction == protocolsession.DirectionReceiverToSender {
		return keys.ReceiverToSender()
	}
	return keys.SenderToReceiver()
}

type FailureSource string

const (
	runtimeFailureSourceRuntime       FailureSource = "runtime"
	runtimeFailureSourceDispatch      FailureSource = "dispatch"
	RuntimeFailureSourceContent       FailureSource = "content"
	RuntimeFailureSourceCatalog       FailureSource = "catalog"
	RuntimeFailureSourceLaneAdmission FailureSource = "lane_admission"
	RuntimeFailureSourcePeer          FailureSource = "peer"
	runtimeFailureSourceLanePump      FailureSource = "lane_pump"
)

type Component struct {
	Source FailureSource
	Run    func(context.Context) error
}

func (runtime *Runtime) start(additional ...Component) {
	components := make([]Component, 0, 1+len(additional))
	components = append(components, Component{runtimeFailureSourceDispatch, runtime.dispatch})
	components = append(components, additional...)
	runtime.work.Add(len(components))
	for _, component := range components {
		go func() {
			defer runtime.work.Done()
			err := component.Run(runtime.ctx)
			if runtime.ctx.Err() != nil {
				return
			}
			// A component has no independent normal terminal state. Even a cancellation
			// error is unexpected while the shared context is live and must keep its cause.
			runtime.terminateWithFailure(TerminationFailed, err, component.Source)
		}()
	}
	runtime.lanes.start()
	go func() {
		runtime.work.Wait()
		runtime.closeExternalAdmissions()
		runtime.lanes.shutdown()
		runtime.finish()
	}()
}

// abortBeforeStart closes construction-time ownership when composition fails
// after keys and channel state exist but before any runtime goroutine starts.
// Keeping this path separate prevents error handling from waiting on a done
// channel that no component could ever close.
func (runtime *Runtime) abortBeforeStart() {
	if runtime == nil {
		return
	}
	runtime.terminate(TerminationFailed)
	runtime.closeExternalAdmissions()
	runtime.lanes.abort()
	runtime.finish()
}

func (runtime *Runtime) finish() {
	runtime.finishOnce.Do(func() {
		runtime.closeExternalAdmissions()
		runtime.router.Close()
		runtime.routes.clear()
		runtime.finalizeMu.Lock()
		runtime.finalizing = true
		finalizers := append([]func(){}, runtime.finalizers...)
		runtime.finalizers = nil
		runtime.finalizeMu.Unlock()
		for _, finalize := range finalizers {
			finalize()
		}
		runtime.keys.Destroy()
		close(runtime.done)
	})
}

func (runtime *Runtime) beginExternalAdmission(
	caller context.Context,
) (context.Context, func(), error) {
	if runtime == nil || caller == nil {
		return nil, nil, sessioncontract.ErrRuntimeConfig
	}
	runtime.externalMu.Lock()
	if runtime.externalClosing || runtime.ctx.Err() != nil {
		runtime.externalMu.Unlock()
		return nil, nil, sessioncontract.ErrRuntimeClosed
	}
	runtime.externalAdmissions.Add(1)
	lifecycle := runtime.ctx
	runtime.externalMu.Unlock()
	ctx, cancel := context.WithCancel(caller)
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stopLifecycle()
			cancel()
			runtime.externalAdmissions.Done()
		})
	}, nil
}

func (runtime *Runtime) closeExternalAdmissions() {
	if runtime == nil {
		return
	}
	runtime.externalMu.Lock()
	runtime.externalClosing = true
	runtime.externalMu.Unlock()
	runtime.externalAdmissions.Wait()
}

func (runtime *Runtime) addFinalizer(finalize func()) error {
	if runtime == nil || finalize == nil {
		return sessioncontract.ErrRuntimeConfig
	}
	runtime.finalizeMu.Lock()
	defer runtime.finalizeMu.Unlock()
	if runtime.finalizing {
		return sessioncontract.ErrRuntimeClosed
	}
	runtime.finalizers = append(runtime.finalizers, finalize)
	return nil
}

func (runtime *Runtime) dispatch(ctx context.Context) error {
	for {
		event, err := runtime.router.Next(ctx)
		if err != nil {
			return err
		}
		if err := runtime.router.Dispatch(ctx, event); err != nil {
			return fmt.Errorf("dispatch authenticated session message: %w", err)
		}
	}
}

func (runtime *Runtime) recordError(err error) {
	runtime.errMu.Lock()
	if runtime.err == nil {
		runtime.err = err
	}
	runtime.errMu.Unlock()
}

func (runtime *Runtime) close() {
	if runtime == nil {
		return
	}
	runtime.beginClose()
	runtime.waitClosed()
}

func (runtime *Runtime) beginClose() {
	if runtime != nil {
		runtime.terminate(TerminationForcedClose)
	}
}

func (runtime *Runtime) waitClosed() {
	if runtime == nil {
		return
	}
	<-runtime.done
}

func (runtime *Runtime) Err() error {
	if runtime == nil {
		return sessioncontract.ErrRuntimeClosed
	}
	runtime.errMu.Lock()
	defer runtime.errMu.Unlock()
	return runtime.err
}

func (runtime *Runtime) Done() <-chan struct{} { return runtime.done }

// Lifetime cancels when session authority ends, before finalizers close Done.
// Dependent owners borrow it to stop waiting and revoke in-flight work while
// the runtime is still joining callbacks and physical resources.
func (runtime *Runtime) Lifetime() context.Context { return runtime.ctx }

func (runtime *Runtime) Stopping() bool {
	if runtime == nil || runtime.ctx == nil {
		return true
	}
	// Cancellation begins shutdown before finalizers can close Done. Admission
	// callers need that earlier boundary so the finalizer gap cannot look live.
	select {
	case <-runtime.ctx.Done():
		return true
	default:
		return false
	}
}

func (runtime *Runtime) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return runtime.sessionID
}

func (runtime *Runtime) LaneIdentity() (uint32, uint32) {
	return runtime.initial.ID, runtime.initial.Epoch
}
