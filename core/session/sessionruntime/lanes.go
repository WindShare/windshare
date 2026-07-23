package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/session/protocolsession"
)

var (
	ErrLaneUnavailable = errors.New("session runtime has no usable lane")
	ErrLaneStale       = errors.New("session runtime lane epoch is stale")
)

// LaneIdentity names one physical FrameChannel incarnation without leaking any
// provider or path metadata into core. Epoch zero is reserved for the transcript
// lane; authenticated attachments always use a positive epoch.
type LaneIdentity struct {
	ID    uint32
	Epoch uint32
}

func (identity LaneIdentity) valid(initial bool) bool {
	return identity.ID != 0 && (initial || identity.Epoch != 0)
}

type runtimeLane struct {
	identity LaneIdentity
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
	completeOnce sync.Once
	started      bool
	closing      bool
}

// selectedLane is an immutable dependency snapshot captured while the lane registry is
// locked. Natural detach may clear the mutable runtimeLane owner immediately
// after selection; callers retain these concrete dependencies long enough to
// observe a stopped writer or closed channel instead of racing a nil reference.
type selectedLane struct {
	identity LaneIdentity
	channel  protocolsession.FrameChannel
	writer   *protocolsession.SessionWriter
	done     <-chan struct{}
}

func (lane *runtimeLane) usableLocked() bool {
	return lane != nil && !lane.closing && lane.writer != nil && lane.writer.Accepting() &&
		lane.channel != nil && lane.done != nil
}

func (lane *runtimeLane) closeChannel() {
	if lane != nil {
		lane.closeChannelReference(lane.channel)
	}
}

func (lane *runtimeLane) closeChannelReference(channel protocolsession.FrameChannel) {
	if lane == nil {
		return
	}
	lane.closeOnce.Do(func() {
		if channel != nil {
			_ = channel.Close()
		}
	})
}

func (lane *runtimeLane) destroyCrypto() {
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

func (lane *runtimeLane) releaseRuntimeReferences() {
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

type runtimeLanePolicy struct{ runtime *runtimeCore }

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
	runtime  *runtimeCore
	identity LaneIdentity
}

type inboundRouteBinding struct {
	ctx          context.Context
	operationID  protocolsession.OperationID
	hasOperation bool
	reserved     *operationLaneRoute
	bound        *operationLaneRoute
	replay       bool
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
	return disposition, err
}

func (router laneInboundRouter) RouteAuthenticatedOperationViolation(
	ctx context.Context,
	message protocolsession.Message,
	violation protocolsession.AuthenticatedOperationViolation,
) (bool, error) {
	return router.runtime.router.RouteAuthenticatedOperationViolation(ctx, message, violation)
}

func (router laneInboundRouter) prepareInboundRoute(
	ctx context.Context,
	message protocolsession.Message,
) (inboundRouteBinding, error) {
	operationID, hasOperation := message.OperationID()
	binding := inboundRouteBinding{ctx: ctx, operationID: operationID, hasOperation: hasOperation}
	if !hasOperation || router.runtime.role != protocolsession.RoleSender {
		return binding, nil
	}
	if receiverRequestKind(message.Kind()) {
		return router.reserveInboundRequest(binding, message)
	}
	binding.bound = router.runtime.routes.current(operationID)
	if binding.bound == nil && message.Kind() != protocolsession.MessageCancel {
		return binding, ErrOperationMissing
	}
	if binding.bound != nil {
		binding.ctx = bindOutboundRoute(ctx, operationID, binding.bound)
	}
	return binding, nil
}

func (router laneInboundRouter) reserveInboundRequest(
	binding inboundRouteBinding,
	message protocolsession.Message,
) (inboundRouteBinding, error) {
	// Reservation precedes queue publication so a fast dispatch worker cannot
	// emit the first fragment before its physical-lane route becomes visible.
	reserved, fresh, err := router.runtime.routes.reserveRequest(
		binding.operationID, router.identity, message,
	)
	if err != nil {
		return binding, err
	}
	binding.reserved = reserved
	if !fresh {
		binding.replay = true
		return binding, nil
	}
	binding.bound = reserved
	binding.ctx = bindOutboundRoute(binding.ctx, binding.operationID, reserved)
	return binding, nil
}

func (router laneInboundRouter) TerminateLocal() error {
	return router.runtime.router.TerminateLocal()
}

func (router laneInboundRouter) InboundDirection() protocolsession.Direction {
	return router.runtime.router.InboundDirection()
}

func receiverRequestKind(kind protocolsession.MessageKind) bool {
	switch kind {
	case protocolsession.MessageListChildren, protocolsession.MessageOpenRevisions,
		protocolsession.MessageRenewLease, protocolsession.MessageReleaseLease,
		protocolsession.MessageRequestBlocks, protocolsession.MessageLaneAttach,
		protocolsession.MessagePeerOffer:
		return true
	default:
		return false
	}
}

func senderResponseFinal(kind protocolsession.MessageKind) bool {
	switch kind {
	case protocolsession.MessageCatalogResult, protocolsession.MessageOpenResults,
		protocolsession.MessageLeaseResult, protocolsession.MessageOperationComplete,
		protocolsession.MessageOperationError, protocolsession.MessageLaneAttach:
		return true
	default:
		return false
	}
}

type runtimeLanes struct {
	runtime *runtimeCore

	mu       sync.Mutex
	active   map[uint32]*runtimeLane
	epochs   map[uint32]uint32
	order    []uint32
	next     uint64
	started  bool
	stopping bool
	wait     sync.WaitGroup
	onDetach func(LaneIdentity)

	terminalMu       sync.Mutex
	terminalAdmitted bool
}

func newRuntimeLanes(runtime *runtimeCore) *runtimeLanes {
	return &runtimeLanes{
		runtime: runtime,
		active:  make(map[uint32]*runtimeLane),
		epochs:  make(map[uint32]uint32),
	}
}

func (lanes *runtimeLanes) add(
	identity LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
	initial bool,
) (*runtimeLane, error) {
	return lanes.addWithAdmission(identity, channel, authenticator, initial, nil)
}

func (lanes *runtimeLanes) addWithAdmission(
	identity LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
	initial bool,
	admit func() error,
) (*runtimeLane, error) {
	if lanes == nil || !identity.valid(initial) || channel == nil || authenticator == nil {
		return nil, ErrRuntimeConfig
	}
	lane, err := lanes.build(identity, channel, authenticator)
	if err != nil {
		return nil, err
	}

	lanes.mu.Lock()
	var rejection error
	switch {
	case lanes.stopping:
		rejection = ErrRuntimeClosed
	case !initial && !lanes.hasUsableLocked():
		rejection = ErrRuntimeClosed
	case lanes.active[identity.ID].usableLocked():
		rejection = ErrLaneUnavailable
	}
	previous, seen := lanes.epochs[identity.ID]
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
	lanes.active[identity.ID] = lane
	lanes.epochs[identity.ID] = identity.Epoch
	if !seen {
		lanes.order = append(lanes.order, identity.ID)
	}
	if admit != nil {
		if err := admit(); err != nil {
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
	identity LaneIdentity,
	channel protocolsession.FrameChannel,
	authenticator protocolsession.InboundMessageAuthenticator,
) (*runtimeLane, error) {
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
	return &runtimeLane{
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

func (lanes *runtimeLanes) startLocked(lane *runtimeLane) {
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

func (lanes *runtimeLanes) run(lane *runtimeLane) {
	defer lanes.wait.Done()
	results := make(chan laneRunResult, 2)
	go func() { results <- laneRunResult{component: "writer", err: lane.writer.Run(lane.ctx)} }()
	go func() { results <- laneRunResult{component: "pump", err: lane.pump.Run(lane.ctx)} }()
	first := <-results
	lanes.markClosing(lane)
	lane.cancel()
	lane.closeChannel()
	second := <-results
	cause := fatalLaneError(first, second)
	lanes.completeLane(lane)

	if cause != nil && lanes.runtime.ctx.Err() == nil {
		lanes.runtime.recordError(cause)
		lanes.runtime.cancel()
	}
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

func (lanes *runtimeLanes) finishLane(lane *runtimeLane) {
	lanes.mu.Lock()
	if lanes.active[lane.identity.ID] == lane {
		delete(lanes.active, lane.identity.ID)
	}
	hook := lanes.onDetach
	lastUsable := !lanes.stopping && !lanes.hasUsableLocked()
	lanes.mu.Unlock()
	if hook != nil {
		hook(lane.identity)
	}
	if lastUsable {
		// Losing every physical path ends this cryptographic session. A later
		// connection must run a fresh transcript and cannot revive old sequence,
		// operation, grant, or lease authority.
		lanes.runtime.cancel()
	}
}

func (lanes *runtimeLanes) completeLane(lane *runtimeLane) {
	if lane == nil {
		return
	}
	lane.completeOnce.Do(func() {
		// Both exclusive crypto consumers have returned—or were never started—so
		// completion can publish detach semantics before releasing opaque owners.
		lane.destroyCrypto()
		lanes.finishLane(lane)
		lane.releaseRuntimeReferences()
		close(lane.done)
	})
}

func (lanes *runtimeLanes) markClosing(lane *runtimeLane) {
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

func (lanes *runtimeLanes) setDetachHook(hook func(LaneIdentity)) {
	lanes.mu.Lock()
	lanes.onDetach = hook
	lanes.mu.Unlock()
}

func (lanes *runtimeLanes) selectLane(preferred *LaneIdentity) (selectedLane, error) {
	if preferred != nil {
		lanes.mu.Lock()
		defer lanes.mu.Unlock()
		return lanes.selectLaneLocked(preferred, nil)
	}
	return lanes.selectLaneExcluding(nil)
}

func (lanes *runtimeLanes) selectLaneExcluding(excluded map[LaneIdentity]struct{}) (selectedLane, error) {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	return lanes.selectLaneLocked(nil, excluded)
}

func (lanes *runtimeLanes) selectLaneLocked(
	preferred *LaneIdentity,
	excluded map[LaneIdentity]struct{},
) (selectedLane, error) {
	if lanes.stopping || lanes.runtime.ctx.Err() != nil {
		return selectedLane{}, ErrRuntimeClosed
	}
	if preferred != nil {
		lane := lanes.active[preferred.ID]
		_, blocked := excluded[*preferred]
		if !lane.usableLocked() || lane.identity != *preferred || blocked {
			return selectedLane{}, ErrLaneUnavailable
		}
		return lane.selected(), nil
	}
	if len(lanes.order) == 0 {
		return selectedLane{}, ErrLaneUnavailable
	}
	for scanned := 0; scanned < len(lanes.order); scanned++ {
		index := int(lanes.next % uint64(len(lanes.order)))
		lanes.next++
		lane := lanes.active[lanes.order[index]]
		if lane.usableLocked() {
			if _, blocked := excluded[lane.identity]; blocked {
				continue
			}
			return lane.selected(), nil
		}
	}
	return selectedLane{}, ErrLaneUnavailable
}

func (lane *runtimeLane) selected() selectedLane {
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

func (lanes *runtimeLanes) detach(identity LaneIdentity) bool {
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
	for _, id := range lanes.order {
		lane := lanes.active[id]
		if lane.usableLocked() {
			result = append(result, lane.selected())
		}
	}
	return result
}

func (lanes *runtimeLanes) shutdown() {
	type ownedLane struct {
		lane    *runtimeLane
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
	lanes.active = make(map[uint32]*runtimeLane)
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
	active := make([]*runtimeLane, 0, len(lanes.active))
	for _, lane := range lanes.active {
		active = append(active, lane)
	}
	lanes.active = make(map[uint32]*runtimeLane)
	lanes.mu.Unlock()
	for _, lane := range active {
		lane.closeChannel()
		lane.releaseRuntimeReferences()
	}
}
