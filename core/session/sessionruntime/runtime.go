// Package sessionruntime composes the transcript, sole pump/writer, role router,
// and business services into one owned ProtocolSession lifecycle.
package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultActiveOperations     = 256
	DefaultOperationTombstones  = 4_096
	SessionStoppedCode          = protocolsession.SessionTerminalCodeLast
	MaximumTerminalMessageBytes = protocolsession.MaxSessionTerminalMessageBytes
)

var (
	ErrRuntimeConfig = errors.New("session runtime configuration is invalid")
	ErrRuntimeClosed = errors.New("session runtime is closed")
	ErrHandshake     = errors.New("session runtime handshake failed")
	ErrScanProgress  = errors.New("session runtime scan progress changed identity or regressed")
)

type lockedReader struct {
	mu     sync.Mutex
	reader io.Reader
}

func (reader *lockedReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reader.Read(destination)
}

type runtimeCore struct {
	peerPathMu              sync.RWMutex
	peerPathHandler         func(context.Context, []byte) error
	share                   catalog.ShareInstance
	role                    protocolsession.Role
	sessionID               protocolsession.ProtocolSessionID
	initial                 LaneIdentity
	keys                    protocolsession.SessionKeys
	random                  io.Reader
	operations              *protocolsession.OperationTable
	router                  *protocolsession.RoleRouter
	lanes                   *runtimeLanes
	routes                  *operationLaneRoutes
	now                     func() time.Time
	protocolTracer          ProtocolOperationTracer
	sessionTerminalObserver SenderSessionTerminalObserver
	termination             runtimeTerminationArbiter

	ctx             context.Context
	cancel          context.CancelFunc
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

type runtimeConfig struct {
	Share                   catalog.ShareInstance
	Role                    protocolsession.Role
	Keys                    protocolsession.SessionKeys
	LaneID                  uint32
	LaneEpoch               uint32
	Channel                 protocolsession.FrameChannel
	Random                  io.Reader
	Authenticator           protocolsession.InboundMessageAuthenticator
	Continuations           protocolsession.OperationContinuationClassifier
	OperationLimits         protocolsession.OperationLimits
	RouterLimits            protocolsession.RouterLimits
	Now                     func() time.Time
	ProtocolTracer          ProtocolOperationTracer
	SessionTerminalObserver SenderSessionTerminalObserver
}

func newRuntime(config runtimeConfig) (*runtimeCore, error) {
	if config.Share.IsZero() || config.Keys.ProtocolSessionID().IsZero() || config.LaneID == 0 ||
		config.Channel == nil || config.Random == nil || config.Authenticator == nil {
		return nil, ErrRuntimeConfig
	}
	if config.OperationLimits == (protocolsession.OperationLimits{}) {
		config.OperationLimits = protocolsession.OperationLimits{
			MaxActive: DefaultActiveOperations, MaxTombstones: DefaultOperationTombstones,
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
	runtime := &runtimeCore{
		share: config.Share, role: config.Role, sessionID: config.Keys.ProtocolSessionID(),
		initial: LaneIdentity{ID: config.LaneID, Epoch: config.LaneEpoch}, keys: config.Keys,
		random: config.Random, operations: operations, router: router,
		routes:                  newOperationLaneRoutes(),
		now:                     config.Now,
		protocolTracer:          config.ProtocolTracer,
		sessionTerminalObserver: config.SessionTerminalObserver,
		ctx:                     ctx,
		cancelLifecycle:         cancelLifecycle,
		done:                    make(chan struct{}),
	}
	if err := router.RegisterHandler(protocolsession.MessagePeerPathControl, peerPathControlHandler{runtime}); err != nil {
		cancelLifecycle()
		return nil, err
	}
	// Legacy package-local cancellation sites fail closed through the arbiter.
	// Causal owners use the explicit trigger methods below.
	runtime.cancel = func() { runtime.terminate(runtimeTerminationFailed) }
	runtime.lanes = newRuntimeLanes(runtime)
	if _, err := runtime.lanes.add(runtime.initial, config.Channel, config.Authenticator, true); err != nil {
		cancelLifecycle()
		config.Keys.Destroy()
		return nil, err
	}
	return runtime, nil
}

func trafficKey(keys protocolsession.SessionKeys, direction protocolsession.Direction) protocolsession.TrafficKey {
	if direction == protocolsession.DirectionReceiverToSender {
		return keys.ReceiverToSender()
	}
	return keys.SenderToReceiver()
}

type runtimeComponent func(context.Context) error

func (runtime *runtimeCore) start(additional ...runtimeComponent) {
	components := make([]runtimeComponent, 0, 1+len(additional))
	components = append(components, runtime.dispatch)
	components = append(components, additional...)
	runtime.work.Add(len(components))
	for _, component := range components {
		go func() {
			defer runtime.work.Done()
			err := component(runtime.ctx)
			if runtime.ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				runtime.terminateRuntimeFailed(err)
				return
			}
			// A component has no independent normal terminal state. Returning while
			// the runtime is live means the shared session can no longer make progress.
			runtime.terminate(runtimeTerminationFailed)
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
func (runtime *runtimeCore) abortBeforeStart() {
	if runtime == nil {
		return
	}
	runtime.cancel()
	runtime.closeExternalAdmissions()
	runtime.lanes.abort()
	runtime.finish()
}

func (runtime *runtimeCore) finish() {
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

func (runtime *runtimeCore) beginExternalAdmission(
	caller context.Context,
) (context.Context, func(), error) {
	if runtime == nil || caller == nil {
		return nil, nil, ErrRuntimeConfig
	}
	runtime.externalMu.Lock()
	if runtime.externalClosing || runtime.ctx.Err() != nil {
		runtime.externalMu.Unlock()
		return nil, nil, ErrRuntimeClosed
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

func (runtime *runtimeCore) closeExternalAdmissions() {
	if runtime == nil {
		return
	}
	runtime.externalMu.Lock()
	runtime.externalClosing = true
	runtime.externalMu.Unlock()
	runtime.externalAdmissions.Wait()
}

func (runtime *runtimeCore) addFinalizer(finalize func()) error {
	if runtime == nil || finalize == nil {
		return ErrRuntimeConfig
	}
	runtime.finalizeMu.Lock()
	defer runtime.finalizeMu.Unlock()
	if runtime.finalizing {
		return ErrRuntimeClosed
	}
	runtime.finalizers = append(runtime.finalizers, finalize)
	return nil
}

func (runtime *runtimeCore) dispatch(ctx context.Context) error {
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

func (runtime *runtimeCore) recordError(err error) {
	runtime.errMu.Lock()
	if runtime.err == nil {
		runtime.err = err
	}
	runtime.errMu.Unlock()
}

func (runtime *runtimeCore) close() {
	if runtime == nil {
		return
	}
	runtime.beginClose()
	runtime.waitClosed()
}

func (runtime *runtimeCore) beginClose() {
	if runtime != nil {
		runtime.terminate(runtimeTerminationForcedClose)
	}
}

func (runtime *runtimeCore) waitClosed() {
	if runtime == nil {
		return
	}
	<-runtime.done
}

func (runtime *runtimeCore) Err() error {
	if runtime == nil {
		return ErrRuntimeClosed
	}
	runtime.errMu.Lock()
	defer runtime.errMu.Unlock()
	return runtime.err
}

func (runtime *runtimeCore) Done() <-chan struct{} { return runtime.done }

func (runtime *runtimeCore) Stopping() bool {
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
func (runtime *runtimeCore) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return runtime.sessionID
}
func (runtime *runtimeCore) LaneIdentity() (uint32, uint32) {
	return runtime.initial.ID, runtime.initial.Epoch
}

func (runtime *runtimeCore) senderControlBase(lane LaneIdentity) protocolsession.ControlBinding {
	return protocolsession.ControlBinding{
		ShareInstance: runtime.share, ProtocolSessionID: runtime.sessionID,
		LaneID: lane.ID, LaneEpoch: lane.Epoch,
		Direction: protocolsession.DirectionSenderToReceiver,
	}
}

type senderOutbound struct {
	runtime    *runtimeCore
	privateKey ed25519.PrivateKey
	observer   SenderTerminalSendObserver
}

func (outbound senderOutbound) sendControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) (resultOutcome protocolsession.SendOutcome, resultErr error) {
	final := senderResponseFinal(kind)
	traceEnabled := outbound.runtime.protocolOperationTracingEnabled()
	var started time.Time
	var deadlineMillis uint64
	var hasDeadline bool
	requestKind := protocolsession.MessageKind(0)
	if traceEnabled {
		started = outbound.runtime.now()
		deadlineMillis, hasDeadline = remainingDeadlineMillis(ctx, started)
		if route, routeErr := outboundRoute(ctx, operationID); routeErr == nil {
			requestKind = route.requestKind
		}
	}
	transaction, err := beginOutboundTransaction(outbound.runtime, ctx, operationID)
	if err != nil {
		if traceEnabled && requestKind != 0 {
			failure, _ := protocolFailureForResponseSend(
				outbound.runtime.sessionID,
				operationID,
				requestKind,
				kind,
				body,
				LaneIdentity{},
				false,
				protocolsession.SendCompletion{
					Settled: true,
					Outcome: protocolsession.SendOutcomeDropped,
				},
			)
			outbound.runtime.traceProtocolOperation(ProtocolOperationTrace{
				Stage:       ProtocolOperationSenderResponseSettled,
				OperationID: operationID, RequestKind: requestKind,
				ResponseKind: kind, HasResponse: true,
				DeadlineRemainingMillis: deadlineMillis, HasDeadline: hasDeadline,
				OperationElapsedMillis: durationMillis(outbound.runtime.now().Sub(started)),
				Failure:                failure,
				Cause:                  protocolOperationCause(err),
			})
		}
		if final {
			// A final response owns operation retirement even when every physical
			// writer became non-accepting before transaction admission. Otherwise
			// the exact route and generation remain live with no delivery path.
			err = errors.Join(err, outbound.runtime.abandonBoundOutboundOperation(ctx, operationID))
		}
		return protocolsession.SendOutcomeDropped, err
	}
	defer transaction.Close()
	if traceEnabled {
		usableAtSelection := outbound.runtime.lanes.usableCount()
		defer func() {
			completion := transaction.lastCompletion
			failure, _ := protocolFailureForResponseSend(
				outbound.runtime.sessionID,
				operationID,
				transaction.route.requestKind,
				kind,
				body,
				transaction.lane.identity,
				transaction.lane.identity.valid(true),
				completion,
			)
			cause := protocolOperationCause(resultErr)
			if cause == ProtocolOperationCauseNone && transaction.attempted &&
				(!completion.Settled || !completion.Admitted || resultOutcome != protocolsession.SendOutcomeDelivered) {
				cause = ProtocolOperationCauseProtocolFailure
			}
			outbound.runtime.traceProtocolOperation(ProtocolOperationTrace{
				Stage:       ProtocolOperationSenderResponseSettled,
				OperationID: operationID, RequestKind: transaction.route.requestKind,
				ResponseKind: kind, HasResponse: true,
				Lane: transaction.lane.identity, HasLane: transaction.lane.identity.valid(true),
				HasSend: transaction.attempted, SendSettled: completion.Settled,
				SendAdmitted: completion.Admitted, SendOutcome: resultOutcome,
				DeadlineRemainingMillis: deadlineMillis, HasDeadline: hasDeadline,
				OperationElapsedMillis: durationMillis(outbound.runtime.now().Sub(started)),
				UsableLanesAtSelection: usableAtSelection,
				Failure:                failure,
				Cause:                  cause,
			})
		}()
	}
	if final {
		defer func() {
			if resultErr == nil && resultOutcome == protocolsession.SendOutcomeDelivered {
				outbound.runtime.routes.releaseRoute(operationID, transaction.route)
				return
			}
			resultErr = errors.Join(
				resultErr,
				outbound.runtime.abandonOutboundOperation(
					operationID, transaction.route, transaction.generation,
				),
			)
		}()
	}
	resultOutcome, resultErr = transaction.Run(ctx, func(
		lane selectedLane,
		permit protocolsession.OutboundReplayPermit,
	) (protocolsession.SendReceipt, error) {
		prepared, prepareErr := protocolsession.PrepareSenderControl(
			outbound.privateKey, outbound.runtime.senderControlBase(lane.identity), kind, &operationID, body,
		)
		if prepareErr != nil {
			return protocolsession.SendReceipt{}, prepareErr
		}
		if !permit.IsZero() {
			return lane.writer.TrySenderControlReplay(prepared, permit)
		}
		return lane.writer.TryAuthorizedSenderControl(prepared, transaction.authority)
	})
	if !final && (ctx.Err() != nil || outbound.runtime.ctx.Err() != nil) {
		resultErr = errors.Join(
			resultErr, outbound.runtime.abandonOutboundOperation(
				operationID, transaction.route, transaction.generation,
			),
		)
	}
	return resultOutcome, resultErr
}

func (outbound senderOutbound) SendControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) (protocolsession.SendOutcome, error) {
	return outbound.sendControl(ctx, kind, operationID, body)
}
