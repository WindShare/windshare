package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"sync"
	"time"
)

var (
	ErrLaneUnavailable = errors.New("session runtime has no usable lane")
	ErrLaneStale       = errors.New("session runtime lane epoch is stale")
)

type RuntimeLane struct {
	requests *requestlane.Lane
	identity session.LaneIdentity
	channel  protocolsession.FrameChannel
	writer   *protocolsession.SessionWriter
	pump     *protocolsession.ProtocolPump
	sealer   *protocolsession.EnvelopeSealer
	opener   *protocolsession.EnvelopeOpener

	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	closeOnce    sync.Once
	cryptoOnce   sync.Once
	retireOnce   sync.Once
	completeOnce sync.Once
	started      bool
	closing      bool
}

// selectedLane is an immutable dependency snapshot captured while the lane registry is
// locked. Natural detach may clear the mutable runtimeLane owner immediately
// after selection; callers retain these concrete dependencies long enough to
// observe a stopped writer or closed channel instead of racing a nil reference.
type selectedLane struct {
	identity session.LaneIdentity
	channel  protocolsession.FrameChannel
	writer   *protocolsession.SessionWriter
	done     <-chan struct{}
}

func (lane *RuntimeLane) usableLocked() bool {
	return lane != nil && !lane.closing && lane.writer != nil && lane.writer.Accepting() &&
		lane.channel != nil && lane.done != nil
}

func (lane *RuntimeLane) closeChannel() {
	if lane != nil {
		lane.closeChannelReference(lane.channel)
	}
}

func (lane *RuntimeLane) closeChannelReference(channel protocolsession.FrameChannel) {
	if lane == nil {
		return
	}
	lane.closeOnce.Do(func() {
		if channel != nil {
			_ = channel.Close()
		}
	})
}

func (lane *RuntimeLane) destroyCrypto() {
	if lane == nil {
		return
	}
	lane.cryptoOnce.Do(func() {
		lane.sealer.Destroy()
		lane.opener.Destroy()
		lane.sealer = nil
		lane.opener = nil
	})
}

func (lane *RuntimeLane) releaseRuntimeReferences() {
	if lane == nil {
		return
	}
	lane.destroyCrypto()
	lane.writer = nil
	lane.pump = nil
	lane.channel = nil
	lane.ctx = nil
	lane.cancel = nil
}

type runtimeLanePolicy struct{ runtime *Runtime }

func (policy runtimeLanePolicy) AdmitOutbound(
	message protocolsession.Message,
	permit protocolsession.OutboundOperationPermit,
) (protocolsession.OutboundAdmission, error) {
	return policy.runtime.router.AdmitOutbound(message, permit)
}

func (policy runtimeLanePolicy) AcceptOutboundReplay(
	message protocolsession.Message,
	permit protocolsession.OutboundReplayPermit,
) (protocolsession.OutboundAdmission, error) {
	return policy.runtime.router.AcceptOutboundReplay(message, permit)
}

func (policy runtimeLanePolicy) AcceptOutboundTerminal() error {
	return policy.runtime.lanes.acceptTerminal()
}

func (policy runtimeLanePolicy) OutboundDirection() protocolsession.Direction {
	return policy.runtime.router.OutboundDirection()
}

type laneInboundRouter struct {
	runtime  *Runtime
	identity session.LaneIdentity
}

func (router laneInboundRouter) RouteInbound(
	ctx context.Context,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	binding, err := router.prepareInboundRoute(ctx, message)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if binding.replay {
		// An exact request replay preserves the first physical authority and is
		// never republished to its handler. A conflicting body was rejected above.
		return protocolsession.OperationDrop, nil
	}
	disposition, err := router.runtime.router.RouteInbound(binding.ctx, message)
	if binding.reserved != nil && (err != nil || disposition != protocolsession.OperationDeliver) {
		router.runtime.routes.releaseRoute(binding.operationID, binding.reserved)
	}
	if binding.hasOperation && message.Kind() == protocolsession.MessageCancel && err == nil &&
		disposition == protocolsession.OperationDeliver && binding.bound != nil {
		router.runtime.routes.releaseRoute(binding.operationID, binding.bound)
	}
	if err == nil && disposition == protocolsession.OperationDeliver &&
		router.runtime.role == protocolsession.RoleSender && receiverRequestKind(message.Kind()) &&
		router.runtime.protocolOperationTracingEnabled() && message.Kind() != protocolsession.MessageRequestBlocks {
		router.runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
			Stage:       sessionobs.ProtocolOperationSenderRequestReceived,
			OperationID: binding.operationID, RequestKind: message.Kind(),
			Lane: router.identity, HasLane: true,
			UsableLanesAtSelection: router.runtime.lanes.usableCount(),
			Cause:                  sessionobs.ProtocolOperationCauseNone,
		})
	}
	return disposition, err
}

func (router laneInboundRouter) RouteAuthenticatedOperationViolation(
	ctx context.Context,
	message protocolsession.Message,
	violation protocolsession.AuthenticatedOperationViolation,
) (bool, error) {
	return router.runtime.router.RouteAuthenticatedOperationViolation(ctx, message, violation)
}

func (router laneInboundRouter) TerminateLocal() error {
	return router.runtime.router.TerminateLocal()
}

func (router laneInboundRouter) InboundDirection() protocolsession.Direction {
	return router.runtime.router.InboundDirection()
}

type runtimeLanes struct {
	runtime *Runtime

	mu     sync.Mutex
	active map[uint32]*RuntimeLane
	// Lane IDs are reusable logical slots, bounded by the issuing LaneRegistry.
	// Retaining their epochs and order across physical detach rejects stale owners
	// and keeps replacement channels selectable without appending duplicate slots.
	latestEpochByID map[uint32]uint32
	logicalOrder    []uint32
	nextSlot        uint64
	started         bool
	stopping        bool
	wait            sync.WaitGroup
	onDetach        func(session.LaneIdentity)
	queuedContent   func(session.LaneIdentity) time.Duration

	terminalMu       sync.Mutex
	terminalAdmitted bool
}

func newRuntimeLanes(runtime *Runtime) *runtimeLanes {
	return &runtimeLanes{
		runtime:         runtime,
		active:          make(map[uint32]*RuntimeLane),
		latestEpochByID: make(map[uint32]uint32),
	}
}

func (lanes *runtimeLanes) add(
	identity session.LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
	initial bool,
) (*RuntimeLane, error) {
	return lanes.addWithAdmission(identity, channel, authenticator, initial, nil)
}

func (lanes *runtimeLanes) addWithAdmission(
	identity session.LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
	initial bool,
	admit func(*RuntimeLane) error,
) (*RuntimeLane, error) {
	if lanes == nil || !identity.Valid(initial) || channel == nil || authenticator == nil {
		return nil, session.ErrRuntimeConfig
	}
	lane, err := lanes.build(identity, channel, authenticator)
	if err != nil {
		return nil, err
	}

	lanes.mu.Lock()
	var rejection error
	switch {
	case lanes.stopping:
		rejection = session.ErrRuntimeClosed
	case !initial && !lanes.hasUsableLocked():
		rejection = session.ErrRuntimeClosed
	case lanes.active[identity.ID].usableLocked():
		rejection = ErrLaneUnavailable
	}
	previous, seen := lanes.latestEpochByID[identity.ID]
	if rejection == nil && seen && identity.Epoch <= previous {
		rejection = ErrLaneStale
	}
	if rejection != nil {
		lanes.mu.Unlock()
		// FrameChannel.Close is application code and may reenter lane inspection.
		// Releasing the registry lock first keeps rejection callback-safe.
		lane.closeChannel()
		lane.releaseRuntimeReferences()
		return nil, rejection
	}
	lane.requests = requestlane.New(requestlane.InitialResponse)
	lanes.active[identity.ID] = lane
	lanes.latestEpochByID[identity.ID] = identity.Epoch
	if !seen {
		lanes.logicalOrder = append(lanes.logicalOrder, identity.ID)
	}
	if admit != nil {
		if err := admit(lane); err != nil {
			delete(lanes.active, identity.ID)
			lanes.mu.Unlock()
			lane.closeChannel()
			lane.releaseRuntimeReferences()
			return nil, err
		}
	}
	if lanes.started {
		lanes.startLocked(lane)
	}
	lanes.mu.Unlock()
	return lane, nil
}

func (lanes *runtimeLanes) build(
	identity session.LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
) (*RuntimeLane, error) {
	outboundDirection, err := lanes.runtime.role.OutboundDirection()
	if err != nil {
		return nil, err
	}
	inboundDirection, _ := lanes.runtime.role.InboundDirection()
	outboundBinding := protocolsession.EnvelopeBinding{
		ShareInstance: lanes.runtime.share, ProtocolSessionID: lanes.runtime.sessionID,
		LaneID: identity.ID, LaneEpoch: identity.Epoch, Direction: outboundDirection,
	}
	inboundBinding := outboundBinding
	inboundBinding.Direction = inboundDirection
	sealer, err := protocolsession.NewEnvelopeSealer(
		trafficKey(lanes.runtime.keys, outboundDirection), outboundBinding, lanes.runtime.random,
	)
	if err != nil {
		return nil, err
	}
	opener, err := protocolsession.NewEnvelopeOpener(trafficKey(lanes.runtime.keys, inboundDirection), inboundBinding)
	if err != nil {
		sealer.Destroy()
		return nil, err
	}
	writer, err := protocolsession.NewSessionWriter(channel, sealer, runtimeLanePolicy{runtime: lanes.runtime})
	if err != nil {
		sealer.Destroy()
		opener.Destroy()
		return nil, err
	}
	pump, err := protocolsession.NewProtocolPump(
		channel, opener, authenticator,
		laneInboundRouter{runtime: lanes.runtime, identity: identity},
	)
	if err != nil {
		sealer.Destroy()
		opener.Destroy()
		return nil, err
	}
	return &RuntimeLane{
		identity: identity, channel: channel, writer: writer, pump: pump,
		sealer: sealer, opener: opener, done: make(chan struct{}),
	}, nil
}

func (lanes *runtimeLanes) start() {
	lanes.mu.Lock()
	if lanes.started || lanes.stopping {
		lanes.mu.Unlock()
		return
	}
	lanes.started = true
	for _, lane := range lanes.active {
		lanes.startLocked(lane)
	}
	lanes.mu.Unlock()
}

func (lanes *runtimeLanes) startLocked(lane *RuntimeLane) {
	if lane.started || !lane.usableLocked() {
		return
	}
	lane.started = true
	lane.ctx, lane.cancel = context.WithCancel(lanes.runtime.ctx)
	lanes.wait.Add(1)
	go lanes.run(lane)
}

type laneRunResult struct {
	component string
	err       error
}

func (lanes *runtimeLanes) run(lane *RuntimeLane) {
	defer lanes.wait.Done()
	results := make(chan laneRunResult, 2)
	go func() { results <- laneRunResult{component: "writer", err: lane.writer.Run(lane.ctx)} }()
	go func() { results <- laneRunResult{component: "pump", err: lane.pump.Run(lane.ctx)} }()
	first := <-results
	lanes.markClosing(lane)
	lane.cancel()
	// Physical shutdown may wait on provider callbacks. It must unblock I/O,
	// but cannot hold a quiescent epoch's admission slot during that drain.
	channel := lane.channel
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		lane.closeChannelReference(channel)
	}()
	second := <-results
	lanes.retireRun(lane, first, second)
	<-drained
	lanes.completeLane(lane)
}

func (lanes *runtimeLanes) retireRun(lane *RuntimeLane, results ...laneRunResult) {
	cause := fatalLaneError(results...)
	if cause != nil && lanes.runtime.ctx.Err() == nil {
		// The pump owns authenticated peer-terminal and local protocol failures.
		// Claim that cause before completeLane can publish last-path retirement.
		trigger := TerminationFailed
		if errors.Is(cause, protocolsession.ErrPeerSessionTerminal) {
			trigger = TerminationPeerTerminal
		}
		lanes.runtime.terminateWithFailure(trigger, cause, runtimeFailureSourceLanePump)
	}
	lanes.retireLane(lane)
}

func fatalLaneError(results ...laneRunResult) error {
	for _, result := range results {
		if result.component != "pump" || result.err == nil || errors.Is(result.err, context.Canceled) {
			continue
		}
		// ProtocolPump returns nil for an ordinary channel close. Every non-nil
		// result therefore means the shared operation authority can no longer
		// trust this session, including future error classes added by the pump.
		return fmt.Errorf("%s failed: %w", result.component, result.err)
	}
	return nil
}

func (lanes *runtimeLanes) finishLane(lane *RuntimeLane) {
	lanes.mu.Lock()
	if lanes.active[lane.identity.ID] == lane {
		delete(lanes.active, lane.identity.ID)
	}
	hook := lanes.onDetach
	lastUsable := !lanes.stopping && !lanes.hasUsableLocked()
	var claim TerminationClaim
	if lastUsable {
		// The registry lock makes the no-path decision atomic with lane removal.
		// Claiming here prevents a later stop or cleanup consequence from replacing
		// the actual session root while callbacks publish detach.
		claim = lanes.runtime.claimTermination(TerminationPathsExhausted)
	}
	lanes.mu.Unlock()
	if hook != nil {
		hook(lane.identity)
	}
	if lastUsable {
		// Losing every physical path ends this cryptographic session. A later
		// connection must run a fresh transcript and cannot revive old sequence,
		// operation, grant, or lease authority.
		lanes.runtime.cancelContext()
		claim.Publish()
	}
}

func (lanes *runtimeLanes) retireLane(lane *RuntimeLane) {
	lane.retireOnce.Do(func() {
		// No old-epoch consumer survives retirement. Physical cleanup remains
		// owned by lane.done and the runtime wait group after membership ends.
		lane.destroyCrypto()
		lanes.finishLane(lane)
	})
}

func (lanes *runtimeLanes) completeLane(lane *RuntimeLane) {
	if lane == nil {
		return
	}
	lane.completeOnce.Do(func() {
		lanes.retireLane(lane)
		lane.releaseRuntimeReferences()
		close(lane.done)
	})
}

func (lanes *runtimeLanes) markClosing(lane *RuntimeLane) {
	lanes.mu.Lock()
	if lanes.active[lane.identity.ID] == lane {
		lane.closing = true
	}
	lanes.mu.Unlock()
}

func (lanes *runtimeLanes) hasUsable() bool {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	return !lanes.stopping && lanes.hasUsableLocked()
}

func (lanes *runtimeLanes) hasUsableLocked() bool {
	for _, lane := range lanes.active {
		if lane.usableLocked() {
			return true
		}
	}
	return false
}

func (lanes *runtimeLanes) usableCount() uint32 {
	if lanes == nil {
		return 0
	}
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if lanes.stopping {
		return 0
	}
	var count uint32
	for _, lane := range lanes.active {
		if lane.usableLocked() && count != ^uint32(0) {
			count++
		}
	}
	return count
}

func (lanes *runtimeLanes) setDetachHook(hook func(session.LaneIdentity)) {
	lanes.mu.Lock()
	lanes.onDetach = hook
	lanes.mu.Unlock()
}

func (lanes *runtimeLanes) selectLane(preferred *session.LaneIdentity) (selectedLane, error) {
	if preferred != nil {
		lanes.mu.Lock()
		defer lanes.mu.Unlock()
		return lanes.selectLaneLocked(preferred, nil)
	}
	return lanes.selectLaneExcluding(nil)
}

func (lanes *runtimeLanes) selectLaneExcluding(excluded map[session.LaneIdentity]struct{}) (selectedLane, error) {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	return lanes.selectLaneLocked(nil, excluded)
}

func (lanes *runtimeLanes) selectLaneLocked(
	preferred *session.LaneIdentity,
	excluded map[session.LaneIdentity]struct{},
) (selectedLane, error) {
	if lanes.stopping || lanes.runtime.ctx.Err() != nil {
		return selectedLane{}, session.ErrRuntimeClosed
	}
	if preferred != nil {
		lane := lanes.active[preferred.ID]
		_, blocked := excluded[*preferred]
		if !lane.usableLocked() || lane.identity != *preferred || blocked {
			return selectedLane{}, ErrLaneUnavailable
		}
		return lane.selected(), nil
	}
	if len(lanes.logicalOrder) == 0 {
		return selectedLane{}, ErrLaneUnavailable
	}
	for scanned := 0; scanned < len(lanes.logicalOrder); scanned++ {
		index := int(lanes.nextSlot % uint64(len(lanes.logicalOrder)))
		lanes.nextSlot++
		lane := lanes.active[lanes.logicalOrder[index]]
		if lane.usableLocked() {
			if _, blocked := excluded[lane.identity]; blocked {
				continue
			}
			return lane.selected(), nil
		}
	}
	return selectedLane{}, ErrLaneUnavailable
}

func (lane *RuntimeLane) selected() selectedLane {
	return selectedLane{
		identity: lane.identity,
		channel:  lane.channel,
		writer:   lane.writer,
		done:     lane.done,
	}
}

func (lanes *runtimeLanes) markSelectedClosing(selected selectedLane) {
	lanes.mu.Lock()
	lane := lanes.active[selected.identity.ID]
	if lane != nil && lane.identity == selected.identity && lane.writer == selected.writer {
		lane.closing = true
	}
	lanes.mu.Unlock()
}

func (lanes *runtimeLanes) detach(identity session.LaneIdentity) bool {
	lanes.mu.Lock()
	lane := lanes.active[identity.ID]
	if lane == nil || lane.identity != identity || lane.closing {
		lanes.mu.Unlock()
		return false
	}
	lane.closing = true
	delete(lanes.active, identity.ID)
	cancel, channel := lane.cancel, lane.channel
	lanes.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	lane.closeChannelReference(channel)
	if lane.started {
		<-lane.done
	} else {
		// No lane runner exists to publish the shared detach transition. Once the
		// registry removes this owner, detach must synchronously complete it.
		lanes.completeLane(lane)
	}
	return true
}

func (lanes *runtimeLanes) len() int {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	return len(lanes.active)
}

func (lanes *runtimeLanes) acceptTerminal() error {
	lanes.terminalMu.Lock()
	defer lanes.terminalMu.Unlock()
	if lanes.terminalAdmitted {
		return nil
	}
	if err := lanes.runtime.router.AcceptOutboundTerminal(); err != nil {
		return err
	}
	lanes.terminalAdmitted = true
	return nil
}

func (lanes *runtimeLanes) snapshot() []selectedLane {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	result := make([]selectedLane, 0, len(lanes.active))
	for _, id := range lanes.logicalOrder {
		lane := lanes.active[id]
		if lane.usableLocked() {
			result = append(result, lane.selected())
		}
	}
	return result
}

func (lanes *runtimeLanes) shutdown() {
	type ownedLane struct {
		lane    *RuntimeLane
		cancel  context.CancelFunc
		channel protocolsession.FrameChannel
	}
	lanes.mu.Lock()
	lanes.stopping = true
	active := make([]ownedLane, 0, len(lanes.active))
	for _, lane := range lanes.active {
		lane.closing = true
		// Snapshot references while the registry lock excludes finishLane. Closing
		// them happens after unlock because FrameChannel.Close may reenter runtime.
		active = append(active, ownedLane{lane: lane, cancel: lane.cancel, channel: lane.channel})
	}
	lanes.active = make(map[uint32]*RuntimeLane)
	lanes.mu.Unlock()
	for _, owned := range active {
		if owned.cancel != nil {
			owned.cancel()
		}
		owned.lane.closeChannelReference(owned.channel)
	}
	lanes.wait.Wait()
	for _, owned := range active {
		owned.lane.releaseRuntimeReferences()
	}
}

func (lanes *runtimeLanes) abort() {
	lanes.mu.Lock()
	lanes.stopping = true
	active := make([]*RuntimeLane, 0, len(lanes.active))
	for _, lane := range lanes.active {
		active = append(active, lane)
	}
	lanes.active = make(map[uint32]*RuntimeLane)
	lanes.mu.Unlock()
	for _, lane := range active {
		lane.closeChannel()
		lane.releaseRuntimeReferences()
	}
}
