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
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultActiveOperations     = 256
	DefaultTrackedOperations    = 4_096
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
	responseSequence        atomic.Uint64
	receiptObservations     observationstream.Producer[protocolsession.SendAttemptSettlement]
	receiptObservationsDone chan struct{}
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
	protocolObservations    observationstream.Producer[ProtocolObservation]
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
	ProtocolObservations    observationstream.Producer[ProtocolObservation]
	SessionTerminalObserver SenderSessionTerminalObserver
}

func newRuntime(config runtimeConfig) (*runtimeCore, error) {
	if config.Share.IsZero() || config.Keys.ProtocolSessionID().IsZero() || config.LaneID == 0 ||
		config.Channel == nil || config.Random == nil || config.Authenticator == nil {
		return nil, ErrRuntimeConfig
	}
	if config.OperationLimits == (protocolsession.OperationLimits{}) {
		config.OperationLimits = protocolsession.OperationLimits{
			MaxActive: DefaultActiveOperations, MaxTracked: DefaultTrackedOperations,
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
		protocolObservations:    config.ProtocolObservations,
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
	runtime.startReceiptObservations()
	return runtime, nil
}

func trafficKey(keys protocolsession.SessionKeys, direction protocolsession.Direction) protocolsession.TrafficKey {
	if direction == protocolsession.DirectionReceiverToSender {
		return keys.ReceiverToSender()
	}
	return keys.SenderToReceiver()
}

type runtimeFailureSource string

const (
	runtimeFailureSourceRuntime       runtimeFailureSource = "runtime"
	runtimeFailureSourceDispatch      runtimeFailureSource = "dispatch"
	runtimeFailureSourceContent       runtimeFailureSource = "content"
	runtimeFailureSourceCatalog       runtimeFailureSource = "catalog"
	runtimeFailureSourceLaneAdmission runtimeFailureSource = "lane_admission"
	runtimeFailureSourcePeer          runtimeFailureSource = "peer"
	runtimeFailureSourceLanePump      runtimeFailureSource = "lane_pump"
)

type runtimeComponent struct {
	source runtimeFailureSource
	run    func(context.Context) error
}

func (runtime *runtimeCore) start(additional ...runtimeComponent) {
	components := make([]runtimeComponent, 0, 1+len(additional))
	components = append(components, runtimeComponent{runtimeFailureSourceDispatch, runtime.dispatch})
	components = append(components, additional...)
	runtime.work.Add(len(components))
	for _, component := range components {
		go func() {
			defer runtime.work.Done()
			err := component.run(runtime.ctx)
			if runtime.ctx.Err() != nil {
				return
			}
			// A component has no independent normal terminal state. Even a cancellation
			// error is unexpected while the shared context is live and must keep its cause.
			runtime.terminateWithFailure(runtimeTerminationFailed, err, component.source)
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
		runtime.finishReceiptObservations()
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
) (protocolsession.ResponseSendResult, error) {
	return outbound.executeResponse(ctx, kind, operationID, protocolErrorForResponse(kind, body), func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		prepared, err := protocolsession.PrepareSenderControl(outbound.privateKey, outbound.runtime.senderControlBase(transaction.lane.identity), kind, &operationID, body)
		if err != nil {
			return nil, err
		}
		initial := transaction.lane.identity
		return func(lane selectedLane, permit protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			control := prepared
			if lane.identity != initial {
				var prepareErr error
				control, prepareErr = protocolsession.PrepareSenderControl(outbound.privateKey, outbound.runtime.senderControlBase(lane.identity), kind, &operationID, body)
				if prepareErr != nil {
					return protocolsession.SendReceipt{}, prepareErr
				}
			}
			if !permit.IsZero() {
				return lane.writer.TrySenderControlReplay(control, permit)
			}
			return lane.writer.TryAuthorizedSenderControl(control, transaction.authority)
		}, nil
	})
}

// Preparation establishes immutable message inputs before the first writer
// attempt, so malformed input never becomes a fabricated receipt.
type responsePreparation func(*outboundTransaction) (outboundLaneAttempt, error)

func (outbound senderOutbound) executeResponse(ctx context.Context, kind protocolsession.MessageKind, operationID protocolsession.OperationID, content ProtocolErrorContent, prepare responsePreparation) (protocolsession.ResponseSendResult, error) {
	runtime := outbound.runtime
	sequence := runtime.responseSequence.Add(1)
	requestKind := protocolsession.MessageKind(0)
	if route, err := outboundRoute(ctx, operationID); err == nil {
		requestKind = route.requestKind
	}
	transaction, err := beginOutboundTransaction(runtime, ctx, operationID)
	final := senderResponseFinal(kind)
	if err != nil {
		end := protocolsession.ResponseSendEndPreparationFailed
		if errors.Is(err, ErrLaneUnavailable) {
			end = protocolsession.ResponseSendEndRouteUnavailable
		} else if errors.Is(err, ErrOperationMissing) {
			end = protocolsession.ResponseSendEndAuthorityUnavailable
		}
		result, _ := protocolsession.NewResponseSendNotStarted(end)
		if final {
			cleanupErr := runtime.abandonBoundOutboundOperation(ctx, operationID)
			result = withOutboundCleanup(result, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
		runtime.traceResponseResult(operationID, requestKind, kind, sequence, content, result)
		return result, err
	}
	transaction.responseSequence = sequence
	transaction.responseKind = kind
	attempt, prepareErr := prepare(transaction)
	var result protocolsession.ResponseSendResult
	if prepareErr != nil {
		result, _ = protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndPreparationFailed)
		err = prepareErr
	} else {
		result, err = transaction.Run(ctx, attempt)
	}
	// Resource retirement is independent of physical evidence. A cleanup error
	// cannot turn transport confirmation into proof that a peer owns no resource.
	if final && err == nil && result.Evidence() == protocolsession.ResponseSendEvidenceTransportConfirmed {
		runtime.routes.releaseRoute(operationID, transaction.route)
		result = result.WithCleanup(protocolsession.SendCleanupRouteReleased)
	} else if final || ctx.Err() != nil || runtime.ctx.Err() != nil {
		cleanupErr := runtime.abandonOutboundOperation(operationID, transaction.route, transaction.generation)
		result = withOutboundCleanup(result, cleanupErr)
		err = errors.Join(err, cleanupErr)
	}
	transaction.Close()
	runtime.traceResponseResult(operationID, transaction.route.requestKind, kind, sequence, content, result)
	return result, err
}
func withOutboundCleanup(result protocolsession.ResponseSendResult, err error) protocolsession.ResponseSendResult {
	if err != nil {
		return result.WithCleanup(protocolsession.SendCleanupFailed)
	}
	return result.WithCleanup(protocolsession.SendCleanupOperationRetired)
}
func (outbound senderOutbound) SendControl(ctx context.Context, kind protocolsession.MessageKind, operationID protocolsession.OperationID, body []byte) (protocolsession.ResponseSendResult, error) {
	return outbound.sendControl(ctx, kind, operationID, body)
}
