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
	framechannel "github.com/windshare/windshare/core/framechannel"
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
	share      catalog.ShareInstance
	role       protocolsession.Role
	sessionID  protocolsession.ProtocolSessionID
	initial    LaneIdentity
	keys       protocolsession.SessionKeys
	random     io.Reader
	operations *protocolsession.OperationTable
	router     *protocolsession.RoleRouter
	lanes      *runtimeLanes
	routes     *operationLaneRoutes

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	work   sync.WaitGroup

	errMu      sync.Mutex
	err        error
	once       sync.Once
	finishOnce sync.Once
	finalizeMu sync.Mutex
	finalizers []func()
	finalizing bool

	externalMu         sync.Mutex
	externalClosing    bool
	externalAdmissions sync.WaitGroup
}

type runtimeConfig struct {
	Share           catalog.ShareInstance
	Role            protocolsession.Role
	Keys            protocolsession.SessionKeys
	LaneID          uint32
	LaneEpoch       uint32
	Channel         protocolsession.FrameChannel
	Random          io.Reader
	Authenticator   protocolsession.InboundMessageAuthenticator
	Continuations   protocolsession.OperationContinuationClassifier
	OperationLimits protocolsession.OperationLimits
	RouterLimits    protocolsession.RouterLimits
	Now             func() time.Time
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
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &runtimeCore{
		share: config.Share, role: config.Role, sessionID: config.Keys.ProtocolSessionID(),
		initial: LaneIdentity{ID: config.LaneID, Epoch: config.LaneEpoch}, keys: config.Keys,
		random: config.Random, operations: operations, router: router,
		routes: newOperationLaneRoutes(), ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	runtime.lanes = newRuntimeLanes(runtime)
	if _, err := runtime.lanes.add(runtime.initial, config.Channel, config.Authenticator, true); err != nil {
		cancel()
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
			if err != nil && !errors.Is(err, context.Canceled) {
				runtime.recordError(err)
			}
			runtime.cancel()
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
		runtime.once.Do(runtime.cancel)
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
	observer   SenderTerminalObserver
}

func (outbound senderOutbound) sendControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) (resultOutcome protocolsession.SendOutcome, resultErr error) {
	final := senderResponseFinal(kind)
	transaction, err := beginOutboundTransaction(outbound.runtime, ctx, operationID)
	if err != nil {
		if final {
			// A final response owns operation retirement even when every physical
			// writer became non-accepting before transaction admission. Otherwise
			// the exact route and generation remain live with no delivery path.
			err = errors.Join(err, outbound.runtime.abandonBoundOutboundOperation(ctx, operationID))
		}
		return protocolsession.SendOutcomeDropped, err
	}
	defer transaction.Close()
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

func (outbound senderOutbound) sendTerminalAll(
	deliveryContext context.Context,
	callerContext context.Context,
	body []byte,
) error {
	return outbound.sendTerminalRecipients(
		deliveryContext,
		callerContext,
		body,
		outbound.runtime.lanes.snapshot(),
	)
}

func (outbound senderOutbound) sendTerminalRecipients(
	deliveryContext context.Context,
	callerContext context.Context,
	body []byte,
	lanes []selectedLane,
) error {
	if len(lanes) == 0 {
		// Last-lane detach removes terminal recipients before it publishes core
		// cancellation. No writer receipt was admitted in this state, so there is
		// no terminal delivery lifecycle to fail. Only the caller's cancellation
		// remains an operation failure; lifecycle cancellation merely reports the
		// same naturally ended transport that produced the empty snapshot.
		return callerContext.Err()
	}
	type terminalReceipt struct {
		lane    LaneIdentity
		receipt protocolsession.SendReceipt
	}
	type terminalCompletion struct {
		lane       LaneIdentity
		completion protocolsession.SendCompletion
	}
	receipts := make([]terminalReceipt, 0, len(lanes))
	var combined error
	var hardAdmissionError error
	onlyStoppedBeforeAdmission := true
	for _, lane := range lanes {
		prepared, err := protocolsession.PrepareSenderControl(
			outbound.privateKey,
			outbound.runtime.senderControlBase(lane.identity),
			protocolsession.MessageSessionTerminal,
			nil,
			body,
		)
		if err == nil {
			var receipt protocolsession.SendReceipt
			receipt, err = lane.writer.TrySenderControl(prepared)
			if err == nil {
				receipts = append(receipts, terminalReceipt{lane: lane.identity, receipt: receipt})
			}
		}
		if err != nil && !errorTreeContainsOnly(err, protocolsession.ErrWriterStopped) {
			onlyStoppedBeforeAdmission = false
			hardAdmissionError = errors.Join(hardAdmissionError, err)
		}
		combined = errors.Join(combined, err)
	}
	if len(receipts) == 0 {
		if err := callerContext.Err(); err != nil {
			return errors.Join(combined, err)
		}
		if onlyStoppedBeforeAdmission && !outbound.runtime.lanes.hasUsable() {
			// A snapshot can retain immutable writer references after natural lane
			// completion. If every writer rejected admission because it had already
			// stopped and no replacement is usable, no terminal lifecycle was born.
			return nil
		}
		return combined
	}
	completions := make([]terminalCompletion, 0, len(receipts))
	for _, pending := range receipts {
		completions = append(completions, terminalCompletion{
			lane:       pending.lane,
			completion: pending.receipt.Await(deliveryContext),
		})
	}
	delivered := false
	noUsableReplacement := !outbound.runtime.lanes.hasUsable()
	for _, settled := range completions {
		completion := settled.completion
		naturallyRetired := terminalCompletionNaturallyRetired(completion, noUsableReplacement)
		observeSenderTerminal(
			outbound.observer,
			outbound.runtime.sessionID,
			settled.lane,
			completion,
			naturallyRetired,
		)
		if naturallyRetired {
			// An admitted receipt can settle after channel retirement or while its
			// owning writer publishes last-lane completion. Neither path reached the
			// wire, so an absent replacement makes the lifecycle naturally complete.
			continue
		}
		if completion.Err == nil && completion.Outcome == protocolsession.SendOutcomeDelivered {
			// Once any attached lane delivers terminal, the peer's monotonic
			// session stop may close its siblings before their receipts settle.
			// Every lane was admitted before waiting, so that close is success,
			// not evidence that terminal fanout was skipped.
			delivered = true
		}
		if completion.Err != nil &&
			(completion.Outcome == protocolsession.SendOutcomeDelivered ||
				(completion.Outcome == protocolsession.SendOutcomeDropped &&
					!errorTreeContainsOnly(
						completion.Err,
						protocolsession.ErrWriterStopped,
						context.Canceled,
						context.DeadlineExceeded,
					))) {
			hardAdmissionError = errors.Join(hardAdmissionError, completion.Err)
		}
		combined = errors.Join(combined, completion.Err)
	}
	if delivered {
		// Delivery on one lane makes sibling receipt failures harmless, but it
		// cannot erase caller cancellation or a local preparation/admission fault.
		return errors.Join(callerContext.Err(), hardAdmissionError)
	}
	return errors.Join(combined, callerContext.Err())
}

func terminalCompletionNaturallyRetired(
	completion protocolsession.SendCompletion,
	noUsableReplacement bool,
) bool {
	if completion.TransportDisposition == framechannel.SendRetired {
		return true
	}
	return noUsableReplacement && completion.Settled &&
		completion.TransportDisposition == 0 &&
		completion.Outcome == protocolsession.SendOutcomeDropped &&
		completion.Err != nil &&
		errorTreeContainsOnly(
			completion.Err,
			protocolsession.ErrWriterStopped,
			context.Canceled,
			context.DeadlineExceeded,
		)
}

func errorTreeContainsOnly(err error, allowed ...error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if child != nil && !errorTreeContainsOnly(child, allowed...) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return errorTreeContainsOnly(wrapped, allowed...)
	}
	for _, candidate := range allowed {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}
