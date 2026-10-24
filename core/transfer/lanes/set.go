// Package lanes owns authenticated block scheduling and the complete lifetime of
// admitted attempts. Selection, retirement and settlement share one state owner.
package lanes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

type laneState struct {
	identity        Identity
	route           Route
	lane            BlockSource
	inflight        uint32
	settlementHolds uint32
	failures        uint32
	performance     lanescheduling.Performance
	sampling        lanescheduling.Sampling
	receive         lanescheduling.ReceiveWindow
	retired         bool
	settled         bool
	settlement      *laneSettlementCounters
	usefulBytes     uint64
	lastUsefulAt    time.Time
	admittedAt      time.Time
}

type Set struct {
	remaining          map[*remainingObserver]struct{}
	outputBytes        uint64
	contentRoutePolicy ContentRoutePolicy
	sessionID          protocolsession.ProtocolSessionID
	now                func() time.Time
	after              func(time.Duration) <-chan time.Time

	lifecycle context.Context
	stop      context.CancelFunc

	mu                 sync.Mutex
	attempts           sync.WaitGroup
	fetches            sync.WaitGroup
	publications       sync.WaitGroup
	closeStarted       bool
	closeDone          chan struct{}
	closed             bool
	lanes              map[uint32]*laneState
	contentSuspensions map[uint32]*contentLaneSuspensionPolicy
	routeSupplies      map[*RouteSupply]struct{}

	exploration        lanescheduling.Exploration
	admissionChanged   chan struct{}
	pending            []*pendingAdmission
	allocationSequence uint64
	settlementProducer observationstream.Producer[SettlementSummary]
	settlementConsumer observationstream.Consumer[SettlementSummary]
	finalSettlements   []*laneState
	contentActivity    [RouteTURN + 1]ContentActivity
	downloadMetrics    *downloadmetrics.Metrics
}

func New(config Config) (*Set, error) {
	if !config.ContentRoutePolicy.valid() {
		return nil, ErrInvalid
	}
	if config.ProtocolSessionID.IsZero() {
		return nil, errors.New("lane set requires a protocol session identity")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.After == nil {
		config.After = time.After
	}
	var settlementProducer observationstream.Producer[SettlementSummary]
	var settlementConsumer observationstream.Consumer[SettlementSummary]
	if config.SettlementObservationCapacity != 0 {
		var err error
		settlementProducer, settlementConsumer, err = observationstream.New[SettlementSummary](
			observationstream.Capacity(config.SettlementObservationCapacity),
		)
		if err != nil {
			return nil, fmt.Errorf("lane settlement observations: %w", err)
		}
	}
	lifecycle, stop := context.WithCancel(context.Background())
	return &Set{
		contentRoutePolicy: config.ContentRoutePolicy,
		sessionID:          config.ProtocolSessionID, now: config.Now, after: config.After,
		lifecycle: lifecycle, stop: stop, lanes: make(map[uint32]*laneState),
		contentSuspensions: make(map[uint32]*contentLaneSuspensionPolicy),
		routeSupplies:      make(map[*RouteSupply]struct{}),
		admissionChanged:   make(chan struct{}),
		settlementProducer: settlementProducer,
		settlementConsumer: settlementConsumer,
		closeDone:          make(chan struct{}),
	}, nil
}

func (s *Set) Add(identity Identity, route Route, lane BlockSource) error {
	if identity.ID == 0 || !route.valid() || lane == nil {
		return ErrInvalid
	}
	state := &laneState{identity: identity, route: route, lane: lane, admittedAt: s.now()}
	if s.settlementObservationsEnabled() {
		state.settlement = &laneSettlementCounters{}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	var retired *SettlementSummary
	if current := s.lanes[identity.ID]; current != nil {
		if identity.Epoch <= current.identity.Epoch {
			s.mu.Unlock()
			return ErrStale
		}
		retired = s.retireLaneLocked(current)
		if retired != nil {
			s.publications.Add(1)
		}
		s.lanes[identity.ID] = state
		s.notifyLaneAvailabilityLocked()
		s.mu.Unlock()
		s.publishRegisteredLaneSettlement(retired)
		return nil
	}
	if _, reattachingHeldLane := s.contentSuspensions[identity.ID]; !reattachingHeldLane && s.logicalLaneCountLocked() == MaxLogicalLanes {
		s.mu.Unlock()
		return ErrBudget
	}
	s.lanes[identity.ID] = state
	s.notifyLaneAvailabilityLocked()
	s.mu.Unlock()
	return nil
}

func (s *Set) Remove(identity Identity) bool {
	if identity.ID == 0 {
		return false
	}
	s.mu.Lock()
	current := s.lanes[identity.ID]
	if current == nil || current.identity != identity {
		s.mu.Unlock()
		return false
	}
	delete(s.lanes, identity.ID)
	retired := s.retireLaneLocked(current)
	if retired != nil {
		s.publications.Add(1)
	}
	s.notifyLaneAvailabilityLocked()
	s.mu.Unlock()
	s.publishRegisteredLaneSettlement(retired)
	return true
}

func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lanes)
}

func (s *Set) notifyLaneAvailabilityLocked() {
	s.observeDownloadAvailabilityLocked()
	s.notifyAdmissionChangedLocked()
}

// Receipt readiness does not change authenticated route availability. Keeping
// these events separate avoids rescanning lanes and locking metrics per fragment.
func (s *Set) notifyAdmissionChangedLocked() {
	close(s.admissionChanged)
	s.admissionChanged = make(chan struct{})
}
