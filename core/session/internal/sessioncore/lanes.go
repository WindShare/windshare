package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

var (
	ErrLaneUnavailable = errors.New("session runtime has no usable lane")
	ErrLaneStale       = errors.New("session runtime lane epoch is stale")
)

type RuntimeLane struct {
	requests  *requestlane.Lane
	identity  session.LaneIdentity
	channel   protocolsession.FrameChannel
	writer    *laneWriter
	validator protocolsession.InboundMessageValidator
	sealer    *protocolsession.EnvelopeSealer
	opener    *protocolsession.EnvelopeOpener

	ctx          context.Context
	cancel       context.CancelFunc
	retired      chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	cryptoOnce   sync.Once
	retireOnce   sync.Once
	completeOnce sync.Once
	started      bool
	closing      bool
}

// selectedLane is an immutable dependency snapshot captured while the lane registry is
// locked. Natural detach may clear the mutable RuntimeLane owner immediately
// after selection; callers retain these concrete dependencies long enough to
// observe a stopped writer or closed channel instead of racing a nil reference.
type selectedLane struct {
	identity session.LaneIdentity
	channel  protocolsession.FrameChannel
	writer   *laneWriter
	retired  <-chan struct{}
	done     <-chan struct{}
}

func (lane *RuntimeLane) usableLocked() bool {
	return lane != nil && !lane.closing && lane.writer != nil && lane.writer.Accepting() &&
		lane.channel != nil && lane.done != nil
}

// activeLocked reports whether this lane still keeps session lifetime alive.
// A lane delivering an admitted terminal frame is no longer usable for new
// traffic, but sibling retirement must not cancel its context before delivery.
func (lane *RuntimeLane) activeLocked() bool {
	return lane != nil && !lane.closing && lane.writer != nil && lane.writer.Active() &&
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
	lane.validator = nil
	lane.channel = nil
	lane.ctx = nil
	lane.cancel = nil
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
	validator protocolsession.InboundMessageValidator,
	initial bool,
) (*RuntimeLane, error) {
	return lanes.addWithAdmission(identity, channel, validator, initial, nil)
}

func (lanes *runtimeLanes) addWithAdmission(
	identity session.LaneIdentity,
	channel protocolsession.FrameChannel,
	validator protocolsession.InboundMessageValidator,
	initial bool,
	admit func(*RuntimeLane) error,
) (*RuntimeLane, error) {
	if lanes == nil || !identity.Valid(initial) || channel == nil || validator == nil {
		return nil, session.ErrRuntimeConfig
	}
	lane, err := lanes.build(identity, channel, validator)
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
	validator protocolsession.InboundMessageValidator,
) (*RuntimeLane, error) {
	if validator == nil {
		return nil, protocolsession.ErrNilRuntimeDependency
	}
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
	writer, err := newLaneWriter(channel, sealer)
	if err != nil {
		sealer.Destroy()
		opener.Destroy()
		return nil, err
	}
	return &RuntimeLane{
		identity: identity, channel: channel, writer: writer, validator: validator,
		sealer: sealer, opener: opener, retired: make(chan struct{}), done: make(chan struct{}),
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
	go func() { results <- laneRunResult{component: "pump", err: lanes.runPump(lane.ctx, lane)} }()
	first := <-results
	lanes.markClosing(lane)
	lane.cancel()
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

func (lanes *runtimeLanes) runPump(ctx context.Context, lane *RuntimeLane) error {
	receive := lane.channel.Recv()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-receive:
			if !ok {
				return nil
			}
			if err := lanes.acceptFrame(lane, frame); err != nil {
				return err
			}
		}
	}
}

func (lanes *runtimeLanes) acceptFrame(lane *RuntimeLane, frame framechannel.Frame) error {
	opened, err := lane.opener.Open(frame)
	if err != nil {
		return lanes.rejectInboundLocally(fmt.Errorf("%w: %w", protocolsession.ErrEnvelopeRejected, err))
	}
	message, err := protocolsession.DecodeMessage(opened.Plaintext)
	if err != nil {
		return lanes.rejectInboundLocally(fmt.Errorf("%w: %w", protocolsession.ErrInboundRejected, err))
	}
	validation, err := lane.validator.ValidateInbound(message)
	if err != nil {
		return lanes.rejectInboundLocally(fmt.Errorf("%w: %w", protocolsession.ErrInboundSemantic, err))
	}
	if validation.HasOperationViolation() {
		violation := validation.OperationViolation()
		bound, routeErr := lanes.runtime.inbound.RouteAuthenticatedOperationViolation(message, violation)
		cause := fmt.Errorf(
			"%w: code=%d bound=%t",
			protocolsession.ErrAuthenticatedOperationViolation,
			violation.Code(),
			bound,
		)
		if routeErr != nil {
			cause = fmt.Errorf("%w: route violation: %w", cause, routeErr)
		}
		if violation.Code() == protocolsession.AuthenticatedOperationViolationMalformedPeerControl &&
			bound && routeErr == nil {
			return nil
		}
		return lanes.rejectInboundLocally(cause)
	}
	disposition, err := lanes.runtime.inbound.RouteInbound(lane.identity, message)
	if err != nil {
		return lanes.rejectInboundLocally(fmt.Errorf("%w: %w", protocolsession.ErrInboundRejected, err))
	}
	if disposition == protocolsession.OperationSessionTerminal {
		return protocolsession.NewPeerSessionTerminalError(message)
	}
	if disposition == protocolsession.OperationBelowReplayWindow {
		lanes.respondBelowReplayWindow(lane.identity, message)
		return nil
	}
	return nil
}

func (lanes *runtimeLanes) respondBelowReplayWindow(preferred session.LaneIdentity, request protocolsession.Message) {
	id, ok := request.OperationID()
	if !ok {
		return
	}
	body, err := protocolsession.EncodeOperationRetry(protocolsession.OperationRetryReasonReplayWindow)
	if err != nil {
		return
	}
	reply, err := protocolsession.NewMessage(protocolsession.MessageOperationRetry, &id, body)
	if err != nil {
		return
	}
	status := sessionobs.SendStatusNotSent
	target := preferred
	// Use non-blocking control enqueue so replying to a delayed request can
	// never stall inbound pumps or deferred promotion behind outbound backpressure.
	// Try the originating lane when still usable; otherwise scan remaining usable
	// lanes without re-selecting it, and drop best-effort when all queues are full.
	if selected, selectErr := lanes.selectLane(&preferred); selectErr == nil &&
		selected.writer != nil && selected.writer.EnqueueControl(reply, nil) == nil {
		target = selected.identity
		status = sessionobs.SendStatusConfirmed
	} else if lanes.runtime.ctx.Err() == nil {
		for _, fallback := range lanes.snapshot() {
			if fallback.identity == preferred || fallback.writer == nil {
				continue
			}
			if fallback.writer.EnqueueControl(reply, nil) == nil {
				target = fallback.identity
				status = sessionobs.SendStatusConfirmed
				break
			}
		}
	}
	if lanes.runtime.protocolOperationTracingEnabled() {
		lanes.runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
			Stage:                  sessionobs.ProtocolOperationSenderRequestBelowWindow,
			OperationID:            id,
			RequestKind:            request.Kind(),
			ResponseKind:           protocolsession.MessageOperationRetry,
			HasResponse:            true,
			Lane:                   target,
			HasLane:                target.Valid(true),
			HasSend:                true,
			SendStatus:             status,
			UsableLanesAtSelection: lanes.usableCount(),
			Cause:                  sessionobs.ProtocolOperationCauseReplayWindow,
		})
	}
}

func (lanes *runtimeLanes) rejectInboundLocally(cause error) error {
	if err := lanes.runtime.operations.TerminateLocal(); err != nil &&
		!errors.Is(err, protocolsession.ErrSessionTerminated) {
		return errors.Join(cause, fmt.Errorf("terminate rejected protocol session: %w", err))
	}
	return cause
}

func (lanes *runtimeLanes) retireRun(lane *RuntimeLane, results ...laneRunResult) {
	cause := fatalLaneError(results...)
	if cause != nil && lanes.runtime.ctx.Err() == nil {
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
	lastActive := !lanes.stopping && !lanes.hasActiveLocked()
	var claim TerminationClaim
	if lastActive {
		claim = lanes.runtime.claimTermination(TerminationPathsExhausted)
	}
	lanes.mu.Unlock()
	if hook != nil {
		hook(lane.identity)
	}
	if lastActive {
		lanes.runtime.cancelContext()
		claim.Publish()
	}
}

func (lanes *runtimeLanes) retireLane(lane *RuntimeLane) {
	lane.retireOnce.Do(func() {
		lane.destroyCrypto()
		lanes.finishLane(lane)
		close(lane.retired)
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

func (lanes *runtimeLanes) hasActiveLocked() bool {
	for _, lane := range lanes.active {
		if lane.activeLocked() {
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

// selectRequestLane picks the lowest-latency lane for managed receiver requests
// under the registry lock so concurrent callers observe each other's pending work.
func (lanes *runtimeLanes) selectRequestLane(
	preferred *session.LaneIdentity,
	kind protocolsession.MessageKind,
) (selectedLane, *requestlane.Reservation, requestlane.Estimate, error) {
	if preferred != nil || !requestlane.Managed(kind) {
		selected, err := lanes.selectLane(preferred)
		return selected, nil, requestlane.Estimate{}, err
	}
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if lanes.stopping || lanes.runtime.ctx.Err() != nil {
		return selectedLane{}, nil, requestlane.Estimate{}, session.ErrRuntimeClosed
	}
	now := lanes.runtime.now()
	var best *RuntimeLane
	var estimate requestlane.Estimate
	for _, id := range lanes.logicalOrder {
		lane := lanes.active[id]
		if !lane.usableLocked() {
			continue
		}
		var queued time.Duration
		if lanes.queuedContent != nil {
			queued = lanes.queuedContent(lane.identity)
		}
		candidate := lane.requests.Estimate(kind, now, queued)
		if best == nil || candidate.Expected < estimate.Expected {
			best, estimate = lane, candidate
		}
	}
	if best == nil {
		return selectedLane{}, nil, requestlane.Estimate{}, ErrLaneUnavailable
	}
	return best.selected(), best.requests.Reserve(kind, now), estimate, nil
}

func (lane *RuntimeLane) selected() selectedLane {
	return selectedLane{
		identity: lane.identity,
		channel:  lane.channel,
		writer:   lane.writer,
		retired:  lane.retired,
		done:     lane.done,
	}
}

func (lanes *runtimeLanes) markSelectedClosing(selected selectedLane) {
	lanes.mu.Lock()
	lane := lanes.active[selected.identity.ID]
	if lane != nil && lane.identity == selected.identity && lane.writer == selected.writer &&
		!lane.writer.DeliveringTerminal() {
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
	lanes.detachAndUnlock(lane)
	return true
}

// Remove this exact epoch before dropping the registry lock. Transport teardown
// can re-enter the runtime or wait for the pump to finish retiring the lane.
func (lanes *runtimeLanes) detachAndUnlock(lane *RuntimeLane) {
	lane.closing = true
	delete(lanes.active, lane.identity.ID)
	cancel, channel := lane.cancel, lane.channel
	lanes.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	lane.closeChannelReference(channel)
	if lane.started {
		<-lane.done
	} else {
		lanes.completeLane(lane)
	}
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
	if lanes.runtime.role != protocolsession.RoleSender {
		return protocolsession.ErrInvalidDirection
	}
	if err := lanes.runtime.operations.TerminateLocal(); err != nil {
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
