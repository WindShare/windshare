package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/observationstream"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"sync"
	"time"
)

type operationLaneRoute struct {
	sendMu      sync.Mutex
	preferred   sessioncontract.LaneIdentity
	requestKind protocolsession.MessageKind
	request     []byte
}

func (routes *operationLaneRoutes) reserveRequest(
	operationID protocolsession.OperationID,
	lane sessioncontract.LaneIdentity,
	message protocolsession.Message,
) (*operationLaneRoute, bool, error) {
	if routes == nil || operationID.IsZero() || !lane.Valid(true) {
		return nil, false, sessioncontract.ErrOperationMissing
	}
	encoded, err := protocolsession.EncodeMessage(message)
	if err != nil {
		return nil, false, err
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if existing := routes.routes[operationID]; existing != nil {
		if bytes.Equal(existing.request, encoded) {
			return existing, false, nil
		}
		return nil, false, protocolsession.ErrOperationIDReused
	}
	route := &operationLaneRoute{preferred: lane, requestKind: message.Kind(), request: encoded}
	routes.routes[operationID] = route
	return route, true, nil
}

type outboundRouteContextKey struct{}

type inboundLaneContextKey struct{}

type outboundRouteBinding struct {
	operationID protocolsession.OperationID
	route       *operationLaneRoute
}

func bindOutboundRoute(
	ctx context.Context,
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, outboundRouteContextKey{}, outboundRouteBinding{
		operationID: operationID, route: route,
	})
}

func outboundRoute(ctx context.Context, operationID protocolsession.OperationID) (*operationLaneRoute, error) {
	if ctx == nil || operationID.IsZero() {
		return nil, sessioncontract.ErrOperationMissing
	}
	binding, ok := ctx.Value(outboundRouteContextKey{}).(outboundRouteBinding)
	if !ok || binding.operationID != operationID || binding.route == nil {
		return nil, sessioncontract.ErrOperationMissing
	}
	return binding.route, nil
}

type authenticatedReceiveContext struct {
	lane       sessioncontract.LaneIdentity
	observedAt time.Time
}

func bindInboundLane(ctx context.Context, lane sessioncontract.LaneIdentity, observedAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, inboundLaneContextKey{}, authenticatedReceiveContext{lane: lane, observedAt: observedAt})
}

func inboundLane(ctx context.Context) (sessioncontract.LaneIdentity, bool) {
	if ctx == nil {
		return sessioncontract.LaneIdentity{}, false
	}
	source, ok := ctx.Value(inboundLaneContextKey{}).(authenticatedReceiveContext)
	return source.lane, ok && source.lane.Valid(true)
}

type operationLaneRoutes struct {
	mu     sync.Mutex
	routes map[protocolsession.OperationID]*operationLaneRoute
}

func newOperationLaneRoutes() *operationLaneRoutes {
	return &operationLaneRoutes{routes: make(map[protocolsession.OperationID]*operationLaneRoute)}
}

func (routes *operationLaneRoutes) reserve(
	operationID protocolsession.OperationID,
	lane sessioncontract.LaneIdentity,
) *operationLaneRoute {
	if routes == nil || operationID.IsZero() || !lane.Valid(true) {
		return nil
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != nil {
		return nil
	}
	route := &operationLaneRoute{preferred: lane}
	routes.routes[operationID] = route
	return route
}

func (routes *operationLaneRoutes) beginSend(
	lanes *runtimeLanes,
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
) (*operationLaneRoute, selectedLane, error) {
	if routes == nil || route == nil || operationID.IsZero() {
		return nil, selectedLane{}, sessioncontract.ErrOperationMissing
	}
	route.sendMu.Lock()
	lane, err := routes.resolveRoute(lanes, operationID, route)
	if err != nil {
		route.sendMu.Unlock()
		return nil, selectedLane{}, err
	}
	return route, lane, nil
}

func (routes *operationLaneRoutes) current(operationID protocolsession.OperationID) *operationLaneRoute {
	if routes == nil || operationID.IsZero() {
		return nil
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	return routes.routes[operationID]
}

func (routes *operationLaneRoutes) resolveRoute(
	lanes *runtimeLanes,
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
) (selectedLane, error) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != route {
		return selectedLane{}, sessioncontract.ErrOperationMissing
	}
	lane, err := lanes.selectLane(&route.preferred)
	if err == nil || !errors.Is(err, ErrLaneUnavailable) {
		return lane, err
	}
	lane, err = lanes.selectLane(nil)
	if err != nil {
		return selectedLane{}, err
	}
	route.preferred = lane.identity
	return lane, nil
}

func (routes *operationLaneRoutes) migrate(
	lanes *runtimeLanes,
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	excluded map[sessioncontract.LaneIdentity]struct{},
) (selectedLane, error) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != route {
		return selectedLane{}, sessioncontract.ErrOperationMissing
	}
	if _, failed := excluded[route.preferred]; !failed {
		lane, err := lanes.selectLane(&route.preferred)
		if err == nil || !errors.Is(err, ErrLaneUnavailable) {
			return lane, err
		}
	}
	lane, err := lanes.selectLaneExcluding(excluded)
	if err != nil {
		return selectedLane{}, err
	}
	route.preferred = lane.identity
	return lane, nil
}

func (routes *operationLaneRoutes) releaseRoute(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
) {
	if routes == nil || route == nil {
		return
	}
	routes.mu.Lock()
	if routes.routes[operationID] == route {
		delete(routes.routes, operationID)
	}
	routes.mu.Unlock()
}

func (routes *operationLaneRoutes) retireRoute(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	retire func() error,
) error {
	if routes == nil || route == nil || retire == nil {
		return sessioncontract.ErrOperationMissing
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != route {
		return sessioncontract.ErrOperationMissing
	}
	// The association remains visible until the operation table has recorded its
	// tombstone, so a same-ID reservation cannot slip between delete and retire.
	err := retire()
	delete(routes.routes, operationID)
	return err
}

func (routes *operationLaneRoutes) len() int {
	if routes == nil {
		return 0
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	return len(routes.routes)
}

func (routes *operationLaneRoutes) clear() {
	if routes == nil {
		return
	}
	routes.mu.Lock()
	clear(routes.routes)
	routes.mu.Unlock()
}

type Call struct {
	id               protocolsession.OperationID
	messages         chan operationResponse
	laneMu           sync.Mutex
	lane             sessioncontract.LaneIdentity
	stateMu          sync.Mutex
	lastResponseLane sessioncontract.LaneIdentity
	closed           bool
	done             chan struct{}
	candidateSend    chan struct{}

	generation protocolsession.OperationGeneration
	authority  protocolsession.OutboundOperationPermit
	request    protocolsession.Message
	replay     protocolsession.OutboundReplayPermit

	requests requestlane.Call

	requestKind            protocolsession.MessageKind
	traceEnabled           bool
	traceStarted           time.Time
	traceDeadlineMillis    uint64
	traceHasDeadline       bool
	traceUsableAtSelection uint32
	traceHasSend           bool
	traceSendSettled       bool
	traceSendAdmitted      bool
	traceSendOutcome       protocolsession.SendOutcome
	traceResponseCount     uint64
	traceResponseKind      protocolsession.MessageKind
	traceHasResponse       bool
	traceHasFinalResponse  bool
	traceCause             sessionobs.ProtocolOperationCause
	traceBlockWait         contentflow.BlockWaitTimeout
	traceEmitted           bool
	traceOwner             *TerminalOwner

	// The admitted continuation bound is operation identity metadata, not live
	// authority. Retaining it after close prevents shutdown timing from changing
	// the public contract of an operation that was already returned to a caller.
	maximumContinuations int
	hasContinuationLimit bool

	authenticatedViolation protocolsession.AuthenticatedOperationViolation
	peerOperation          OperationObserver
}

func (call *Call) observeAuthenticatedOperationViolation(
	violation protocolsession.AuthenticatedOperationViolation,
) {
	if call == nil || !validAuthenticatedOperationViolationCode(violation.Code()) {
		return
	}
	call.stateMu.Lock()
	if validAuthenticatedOperationViolationCode(call.authenticatedViolation.Code()) {
		call.stateMu.Unlock()
		return
	}
	call.authenticatedViolation = violation
	operation := call.peerOperation
	call.stateMu.Unlock()
	if operation != nil {
		operation.ObserveAuthenticatedOperationViolation(violation)
	}
}

func (call *Call) BindOperationObserver(operation OperationObserver) error {
	if call == nil || operation == nil {
		return sessioncontract.ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.peerOperation != nil {
		call.stateMu.Unlock()
		return sessioncontract.ErrOperationMissing
	}
	// The same owner receives authenticated violations and settles observation.
	// RPC sink closure only wakes Receive; its joined termination decides whether
	// that wakeup was a local stop or accompanied a real protocol failure.
	call.peerOperation = operation
	operation.BindTerminalOwner(call.claimProtocolTerminationLocked())
	violation := call.authenticatedViolation
	call.stateMu.Unlock()
	if validAuthenticatedOperationViolationCode(violation.Code()) {
		operation.ObserveAuthenticatedOperationViolation(violation)
	}
	return nil
}

func validAuthenticatedOperationViolationCode(
	code protocolsession.AuthenticatedOperationViolationCode,
) bool {
	switch code {
	case protocolsession.AuthenticatedOperationViolationMalformedFailure,
		protocolsession.AuthenticatedOperationViolationMalformedPeerControl,
		protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer,
		protocolsession.AuthenticatedOperationViolationConflictingFinal,
		protocolsession.AuthenticatedOperationViolationContinuationContract:
		return true
	default:
		return false
	}
}

func (call *Call) acquireCandidateSend(
	ctx context.Context,
	lifetime context.Context,
) (func(), error) {
	if call == nil || ctx == nil || lifetime == nil {
		return nil, sessioncontract.ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.closed {
		call.stateMu.Unlock()
		return nil, sessioncontract.ErrOperationMissing
	}
	if call.candidateSend == nil {
		call.candidateSend = make(chan struct{}, 1)
		call.candidateSend <- struct{}{}
	}
	gate := call.candidateSend
	done := call.done
	if done == nil {
		done = make(chan struct{})
		call.done = done
	}
	call.stateMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lifetime.Done():
		return nil, sessioncontract.ErrRuntimeClosed
	case <-done:
		return nil, sessioncontract.ErrOperationMissing
	case <-gate:
	}
	var once sync.Once
	release := func() { once.Do(func() { gate <- struct{}{} }) }
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-lifetime.Done():
		release()
		return nil, sessioncontract.ErrRuntimeClosed
	case <-done:
		release()
		return nil, sessioncontract.ErrOperationMissing
	default:
		return release, nil
	}
}

func (call *Call) doneChannel() <-chan struct{} {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.done == nil {
		call.done = make(chan struct{})
		if call.closed {
			close(call.done)
		}
	}
	return call.done
}

func (call *Call) enqueue(response operationResponse) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return nil
	}
	call.traceResponse(response.message.Kind())
	return call.enqueueLocked(response)
}

type authenticatedFailureSource struct {
	observations      observationstream.Producer[sessionobs.ProtocolObservation]
	observedAt        time.Time
	protocolSessionID protocolsession.ProtocolSessionID
	lane              sessioncontract.LaneIdentity
	hasLane           bool
}

func (call *Call) enqueueAuthenticatedFailure(
	response operationResponse,
	source authenticatedFailureSource,
) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return nil
	}
	call.traceResponse(response.message.Kind())
	call.traceAuthenticatedFailure(response.message, source)
	return call.enqueueLocked(response)
}

func (call *Call) enqueueLocked(response operationResponse) error {
	select {
	case call.messages <- response:
		return nil
	default:
		return sessioncontract.ErrOperationOverflow
	}
}

func (call *Call) traceResponse(kind protocolsession.MessageKind) {
	if !call.traceEnabled {
		return
	}
	if call.traceResponseCount != ^uint64(0) {
		call.traceResponseCount++
	}
	call.traceResponseKind = kind
	call.traceHasResponse = true
	if senderResponseFinal(kind) {
		call.traceHasFinalResponse = true
	}
}

func (call *Call) traceAuthenticatedFailure(
	message protocolsession.Message,
	source authenticatedFailureSource,
) {
	if !call.traceEnabled {
		return
	}
	content, ok := sessionobs.ProtocolErrorForAuthenticatedReceive(message)
	if !ok {
		return
	}
	if source.hasLane && !source.observations.IsZero() {
		source.observations.TryPublish(sessionobs.NewReceivedProtocolError(
			sessionobs.ProtocolObservationContext{Correlation: sessionobs.ProtocolObservationCorrelation{Role: protocolsession.RoleReceiver, ProtocolSessionID: source.protocolSessionID, OperationID: call.id, RequestKind: call.requestKind}, ObservedAt: source.observedAt},
			content, source.lane))
	}
	call.traceCause = call.traceCause.Merge(sessionobs.ProtocolOperationCauseProtocolFailure)
}

func (call *Call) close() {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return
	}
	call.closed = true
	call.requests.Close()
	call.generation = protocolsession.OperationGeneration{}
	call.authority = protocolsession.OutboundOperationPermit{}
	call.request = protocolsession.Message{}
	call.replay = protocolsession.OutboundReplayPermit{}
	if call.done == nil {
		call.done = make(chan struct{})
	}
	close(call.done)
	for {
		select {
		case <-call.messages:
		default:
			return
		}
	}
}

func (call *Call) setAuthority(
	generation protocolsession.OperationGeneration,
	authority protocolsession.OutboundOperationPermit,
) bool {
	maximumContinuations, hasContinuationLimit := generation.MaximumContinuations()
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return false
	}
	call.generation = generation
	call.authority = authority
	call.maximumContinuations = maximumContinuations
	call.hasContinuationLimit = hasContinuationLimit
	return true
}

func (call *Call) operationAuthority() (
	protocolsession.OperationGeneration,
	protocolsession.OutboundOperationPermit,
) {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.generation, call.authority
}

func (call *Call) continuationLimit() (int, bool) {
	if call == nil {
		return 0, false
	}
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.maximumContinuations, call.hasContinuationLimit
}

func (call *Call) setRequestReplay(
	request protocolsession.Message,
	permit protocolsession.OutboundReplayPermit,
) bool {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed || permit.IsZero() {
		return false
	}
	call.request = request
	call.replay = permit
	return true
}

func (call *Call) queueRequestReplay(writer *protocolsession.SessionWriter) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return sessioncontract.ErrOperationMissing
	}
	if call.replay.IsZero() {
		return nil
	}
	if writer == nil {
		return ErrLaneUnavailable
	}
	if _, err := writer.TryControlReplay(call.request, call.replay); err != nil {
		return err
	}
	// Queue admission does not prove physical delivery. Retaining the exact,
	// generation-bound capability lets every later lane migration re-establish
	// the offer dependency before publishing a candidate on that lane.
	return nil
}

type inboundRouteBinding struct {
	ctx          context.Context
	operationID  protocolsession.OperationID
	hasOperation bool
	reserved     *operationLaneRoute
	bound        *operationLaneRoute
	replay       bool
}

func (router laneInboundRouter) prepareInboundRoute(
	ctx context.Context,
	message protocolsession.Message,
) (inboundRouteBinding, error) {
	operationID, hasOperation := message.OperationID()
	binding := inboundRouteBinding{ctx: ctx, operationID: operationID, hasOperation: hasOperation}
	if router.runtime.role == protocolsession.RoleReceiver &&
		(message.Kind() == protocolsession.MessageOperationComplete ||
			message.Kind() == protocolsession.MessageOperationError && router.runtime.protocolOperationTracingEnabled()) {
		// A migrated final can overtake fragments accepted by a retired carrier.
		// Retain its authenticated origin without allocating for every fragment.
		binding.ctx = bindInboundLane(binding.ctx, router.identity, router.runtime.now())
	}
	if !hasOperation || router.runtime.role != protocolsession.RoleSender {
		return binding, nil
	}
	if receiverRequestKind(message.Kind()) {
		return router.reserveInboundRequest(binding, message)
	}
	// Response routes end with the final send or cancellation, while operation
	// tombstones still classify in-flight continuations. A missing response route
	// cannot reject ingress before the operation table checks that authority.
	binding.bound = router.runtime.routes.current(operationID)
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
