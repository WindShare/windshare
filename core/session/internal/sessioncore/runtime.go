package sessioncore

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

type LockedReader struct {
	mu     sync.Mutex
	reader io.Reader
}

func NewLockedReader(reader io.Reader) *LockedReader { return &LockedReader{reader: reader} }

func (reader *LockedReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reader.Read(destination)
}

type Runtime struct {
	peerPathControls     sessionControlHandler
	share                catalog.ShareInstance
	role                 protocolsession.Role
	sessionID            protocolsession.ProtocolSessionID
	initial              session.LaneIdentity
	keys                 protocolsession.SessionKeys
	random               io.Reader
	operations           *operationTable
	inbound              *inboundQueue
	lanes                *runtimeLanes
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
	Validator            protocolsession.InboundMessageValidator
	Continuations        protocolsession.OperationContinuationClassifier
	OperationLimits      protocolsession.OperationLimits
	RouterLimits         protocolsession.RouterLimits
	Now                  func() time.Time
	ProtocolObservations observationstream.Producer[sessionobs.ProtocolObservation]
	TerminationObserver  TerminationObserver
}

func New(config Config) (*Runtime, error) {
	if config.Share.IsZero() || config.Keys.ProtocolSessionID().IsZero() || config.LaneID == 0 ||
		config.Channel == nil || config.Random == nil || config.Validator == nil {
		return nil, session.ErrRuntimeConfig
	}
	if config.OperationLimits == (protocolsession.OperationLimits{}) {
		config.OperationLimits = protocolsession.OperationLimits{
			MaxActive: session.DefaultActiveOperations, MaxTracked: session.DefaultTrackedOperations,
		}
	}
	if config.RouterLimits == (protocolsession.RouterLimits{}) {
		config.RouterLimits = protocolsession.DefaultRouterLimits
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	operations, err := newOperationTable(config.OperationLimits, config.Now, config.Continuations)
	if err != nil {
		return nil, err
	}
	ctx, cancelLifecycle := context.WithCancel(context.Background())
	runtime := &Runtime{
		share:                config.Share,
		role:                 config.Role,
		sessionID:            config.Keys.ProtocolSessionID(),
		initial:              session.LaneIdentity{ID: config.LaneID, Epoch: config.LaneEpoch},
		keys:                 config.Keys,
		random:               config.Random,
		operations:           operations,
		now:                  config.Now,
		protocolObservations: config.ProtocolObservations,
		terminationObserver:  config.TerminationObserver,
		ctx:                  ctx,
		cancelLifecycle:      cancelLifecycle,
		done:                 make(chan struct{}),
	}
	inbound, err := newInboundQueue(runtime, config.Role, operations, config.RouterLimits)
	if err != nil {
		cancelLifecycle()
		return nil, err
	}
	runtime.inbound = inbound
	if err := inbound.RegisterHandler(protocolsession.MessagePeerPathControl, &runtime.peerPathControls); err != nil {
		cancelLifecycle()
		return nil, err
	}
	runtime.lanes = newRuntimeLanes(runtime)
	if _, err := runtime.lanes.add(runtime.initial, config.Channel, config.Validator, true); err != nil {
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

func (runtime *Runtime) Start(additional ...Component) {
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

// AbortBeforeStart closes construction-time ownership when composition fails
// after keys and channel state exist but before any runtime goroutine starts.
func (runtime *Runtime) AbortBeforeStart() {
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
		runtime.inbound.Close()
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

func (runtime *Runtime) BeginExternalAdmission(
	caller context.Context,
) (context.Context, func(), error) {
	if runtime == nil || caller == nil {
		return nil, nil, session.ErrRuntimeConfig
	}
	runtime.externalMu.Lock()
	if runtime.externalClosing || runtime.ctx.Err() != nil {
		runtime.externalMu.Unlock()
		return nil, nil, session.ErrRuntimeClosed
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

func (runtime *Runtime) AddFinalizer(finalize func()) error {
	if runtime == nil || finalize == nil {
		return session.ErrRuntimeConfig
	}
	runtime.finalizeMu.Lock()
	defer runtime.finalizeMu.Unlock()
	if runtime.finalizing {
		return session.ErrRuntimeClosed
	}
	runtime.finalizers = append(runtime.finalizers, finalize)
	return nil
}

func (runtime *Runtime) RegisterHandler(
	kind protocolsession.MessageKind,
	handler protocolsession.MessageHandler,
) error {
	return runtime.inbound.RegisterHandler(kind, handler)
}

func (runtime *Runtime) dispatch(ctx context.Context) error {
	for {
		event, err := runtime.inbound.Next(ctx)
		if err != nil {
			return err
		}
		if err := runtime.inbound.Dispatch(ctx, event); err != nil {
			return fmt.Errorf("dispatch authenticated session message: %w", err)
		}
	}
}

func (runtime *Runtime) RecordError(err error) {
	runtime.errMu.Lock()
	if runtime.err == nil {
		runtime.err = err
	}
	runtime.errMu.Unlock()
}

func (runtime *Runtime) Fail(err error) {
	runtime.terminateRuntimeFailed(err)
}

func (runtime *Runtime) FailProtocol(err error) {
	_ = runtime.operations.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
}

func (runtime *Runtime) FailOperationAuthority() error {
	return runtime.failRPCOperationAuthority()
}

func (runtime *Runtime) Close() {
	if runtime == nil {
		return
	}
	runtime.BeginClose()
	runtime.WaitClosed()
}

func (runtime *Runtime) BeginClose() {
	if runtime != nil {
		runtime.terminate(TerminationForcedClose)
	}
}

func (runtime *Runtime) WaitClosed() {
	if runtime == nil {
		return
	}
	<-runtime.done
}

func (runtime *Runtime) ClaimTermination(cause TerminationCause) TerminationClaim {
	return runtime.claimTermination(cause)
}

func (runtime *Runtime) CancelLifetime() { runtime.cancelContext() }

func (runtime *Runtime) PathsExhausted() bool {
	runtime.termination.mu.Lock()
	defer runtime.termination.mu.Unlock()
	return runtime.termination.claimed && runtime.termination.cause == TerminationPathsExhausted
}

func (runtime *Runtime) Err() error {
	if runtime == nil {
		return session.ErrRuntimeClosed
	}
	runtime.errMu.Lock()
	defer runtime.errMu.Unlock()
	return runtime.err
}

func (runtime *Runtime) Done() <-chan struct{} { return runtime.done }

// Lifetime cancels when session authority ends, before finalizers close Done.
func (runtime *Runtime) Lifetime() context.Context { return runtime.ctx }

func (runtime *Runtime) Stopping() bool {
	if runtime == nil || runtime.ctx == nil {
		return true
	}
	select {
	case <-runtime.ctx.Done():
		return true
	default:
		return false
	}
}

func (runtime *Runtime) ShareInstance() catalog.ShareInstance { return runtime.share }

func (runtime *Runtime) Now() time.Time { return runtime.now() }

func (runtime *Runtime) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return runtime.sessionID
}

func (runtime *Runtime) LaneIdentity() (uint32, uint32) {
	return runtime.initial.ID, runtime.initial.Epoch
}

func (runtime *Runtime) HasUsableLanes() bool { return runtime.lanes.hasUsable() }

func (runtime *Runtime) DetachLane(identity session.LaneIdentity) bool {
	return runtime.lanes.detach(identity)
}

func (runtime *Runtime) AttachedLanes() int { return runtime.lanes.len() }

func (runtime *Runtime) LaneDone(identity session.LaneIdentity) (<-chan struct{}, bool) {
	runtime.lanes.mu.Lock()
	defer runtime.lanes.mu.Unlock()
	lane := runtime.lanes.active[identity.ID]
	if lane == nil || lane.identity != identity {
		return nil, false
	}
	return lane.done, true
}

// ConfigureLanes binds role dependencies before Start.
func (runtime *Runtime) ConfigureLanes(
	queued func(session.LaneIdentity) time.Duration,
	detached func(session.LaneIdentity),
) {
	runtime.lanes.mu.Lock()
	defer runtime.lanes.mu.Unlock()
	runtime.lanes.queuedContent = queued
	runtime.lanes.onDetach = detached
}

// InstallLane publishes both protocol and role ownership under the same registry lock.
func (runtime *Runtime) InstallLane(
	identity session.LaneIdentity,
	channel protocolsession.FrameChannel,
	validator protocolsession.InboundMessageValidator,
	requests *requestlane.Lane,
	admit func() error,
) error {
	_, err := runtime.lanes.addWithAdmission(identity, channel, validator, false, func(lane *RuntimeLane) error {
		if requests != nil {
			lane.requests = requests
		}
		if admit != nil {
			return admit()
		}
		return nil
	})
	return err
}

func (runtime *Runtime) NewLaneHello(grant protocolsession.LaneGrant) (protocolsession.LaneHello, error) {
	return protocolsession.NewLaneHello(
		runtime.share, runtime.sessionID, grant.LaneID, grant.LaneEpoch,
		grant.OperationID, grant.AttachNonce[:], runtime.keys.ReceiverToSender(),
	)
}

func (runtime *Runtime) NewLaneNonce() ([]byte, error) {
	return ReadNonzeroLaneNonce(runtime.random, protocolsession.LaneSenderNonceBytes)
}

func (runtime *Runtime) TracingEnabled() bool { return runtime.protocolOperationTracingEnabled() }

func (runtime *Runtime) ObservationContext(
	id protocolsession.OperationID,
	kind protocolsession.MessageKind,
) sessionobs.ProtocolObservationContext {
	return runtime.observationContext(id, kind)
}

func (runtime *Runtime) Observe(event sessionobs.ProtocolObservation) {
	runtime.protocolObservations.TryPublish(event)
}
