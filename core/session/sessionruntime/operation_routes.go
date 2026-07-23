package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationLaneRoute struct {
	sendMu    sync.Mutex
	preferred LaneIdentity
	request   []byte
}

func (routes *operationLaneRoutes) reserveRequest(
	operationID protocolsession.OperationID,
	lane LaneIdentity,
	message protocolsession.Message,
) (*operationLaneRoute, bool, error) {
	if routes == nil || operationID.IsZero() || !lane.valid(true) {
		return nil, false, ErrOperationMissing
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
	route := &operationLaneRoute{preferred: lane, request: encoded}
	routes.routes[operationID] = route
	return route, true, nil
}

type outboundRouteContextKey struct{}

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
		return nil, ErrOperationMissing
	}
	binding, ok := ctx.Value(outboundRouteContextKey{}).(outboundRouteBinding)
	if !ok || binding.operationID != operationID || binding.route == nil {
		return nil, ErrOperationMissing
	}
	return binding.route, nil
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
	lane LaneIdentity,
) *operationLaneRoute {
	if routes == nil || operationID.IsZero() || !lane.valid(true) {
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
		return nil, selectedLane{}, ErrOperationMissing
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
		return selectedLane{}, ErrOperationMissing
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
	excluded map[LaneIdentity]struct{},
) (selectedLane, error) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != route {
		return selectedLane{}, ErrOperationMissing
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
		return ErrOperationMissing
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.routes[operationID] != route {
		return ErrOperationMissing
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
