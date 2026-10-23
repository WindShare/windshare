package lanes

type contentLaneSuspensionPolicy struct {
	laneID  uint32
	resumed bool
}

// ContentSuspension is an epoch-stable content-admission capability for
// one authenticated logical lane. Its opaque policy identity prevents an old
// handle from releasing a newer hold on the same lane ID.
type ContentSuspension struct {
	lanes  *Set
	policy *contentLaneSuspensionPolicy
}

// Route is the authenticated content route admitted for one lane
// incarnation. It is captured at admission because lane numbers carry no
// transport meaning and may be reused across epochs.
type Route uint8

const (
	RouteRelay Route = iota + 1
	RouteDirect
	RouteTURN
)

func (route Route) valid() bool {
	return route == RouteRelay || route == RouteDirect || route == RouteTURN
}

// ContentRoutePolicy is fixed before a session admits content. Keeping it on
// the set means replacement epochs and new lane IDs cannot bypass user intent.
type ContentRoutePolicy uint8

const (
	ContentRouteAll ContentRoutePolicy = iota
	ContentRouteDirectOnly
	ContentRouteRelayOnly
)

func (policy ContentRoutePolicy) valid() bool { return policy <= ContentRouteRelayOnly }

func (policy ContentRoutePolicy) Allows(route Route) bool {
	if !route.valid() {
		return false
	}
	switch policy {
	case ContentRouteAll:
		return true
	case ContentRouteDirectOnly:
		return route == RouteDirect
	case ContentRouteRelayOnly:
		return route == RouteRelay
	default:
		return false
	}
}

func (s *Set) logicalLaneCountLocked() int {
	count := len(s.lanes)
	for laneID := range s.contentSuspensions {
		if s.lanes[laneID] == nil {
			count++
		}
	}
	return count
}

// SuspendContent removes an authenticated logical lane from content admission
// without detaching its control transport. The initial exact identity prevents
// suspending an unintended incarnation, while the returned capability follows
// replacements because reconnects must not bypass an active admission policy.
func (s *Set) SuspendContent(identity Identity) (*ContentSuspension, error) {
	if identity.ID == 0 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity {
		return nil, ErrStale
	}
	if _, exists := s.contentSuspensions[identity.ID]; exists {
		return nil, ErrInvalid
	}
	policy := &contentLaneSuspensionPolicy{laneID: identity.ID}
	s.contentSuspensions[identity.ID] = policy
	s.notifyLaneAvailabilityLocked()
	return &ContentSuspension{lanes: s, policy: policy}, nil
}

// Resume releases only the hold represented by this capability. It is
// idempotent so concurrent admission signals cannot release a later policy.
func (suspension *ContentSuspension) Resume() error {
	if suspension == nil || suspension.lanes == nil || suspension.policy == nil {
		return ErrInvalid
	}
	lanes := suspension.lanes
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if suspension.policy.resumed {
		return nil
	}
	if lanes.closed {
		return ErrClosed
	}
	current := lanes.contentSuspensions[suspension.policy.laneID]
	if current != suspension.policy {
		return ErrStale
	}
	suspension.policy.resumed = true
	delete(lanes.contentSuspensions, suspension.policy.laneID)
	lanes.notifyLaneAvailabilityLocked()
	return nil
}
