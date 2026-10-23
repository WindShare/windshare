package lanes

import (
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
)

// BindDownloadMetrics is called before content activation, once per generation.
func (s *Set) BindDownloadMetrics(metrics *downloadmetrics.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadMetrics = metrics
	s.observeDownloadAvailabilityLocked()
}

func (s *Set) observeDownloadAvailabilityLocked() {
	if s.downloadMetrics == nil {
		return
	}
	direct := false
	for _, state := range s.lanes {
		if !state.retired && state.route == RouteDirect && s.contentSuspensions[state.identity.ID] == nil {
			direct = true
		}
	}
	s.downloadMetrics.Availability(direct)
}

func (s *Set) attestBlock(state *laneState, record records.BlockRecord) Block {
	s.mu.Lock()
	defer s.mu.Unlock()
	route := downloadmetrics.Unknown
	if !state.retired && s.lanes[state.identity.ID] == state {
		switch state.route {
		case RouteDirect:
			route = downloadmetrics.Direct
		case RouteTURN:
			route = downloadmetrics.TURN
		case RouteRelay:
			route = downloadmetrics.ApplicationRelay
		}
	}
	return Block{BlockRecord: record, route: route}
}

// ContentActivity counts only selected authenticated block results. Open
// channels, discarded race losers and ciphertext never imply useful traffic.
type ContentActivity struct {
	Route         Route
	UsefulBytes   uint64
	LastUsefulAt  time.Time
	AdmittedLanes uint32
}

func (s *Set) recordContentActivityLocked(route Route, bytes uint64) {
	activity := &s.contentActivity[route]
	activity.Route = route
	var incomplete bool
	saturatingLaneCounter(&activity.UsefulBytes, bytes, &incomplete)
	activity.LastUsefulAt = s.now()
}

func (s *Set) ContentActivity() []ContentActivity {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ContentActivity, 0, int(RouteTURN))
	activityByRoute := s.contentActivity
	for _, lane := range s.lanes {
		if !lane.retired {
			activityByRoute[lane.route].Route = lane.route
			activityByRoute[lane.route].AdmittedLanes++
		}
	}
	for _, activity := range activityByRoute {
		if activity.UsefulBytes != 0 || activity.AdmittedLanes != 0 {
			result = append(result, activity)
		}
	}
	return result
}

const UnhelpfulLaneWindow = 30 * time.Second

const UnhelpfulLaneRateRatio = 1.5

type Measurement struct {
	UsefulBytes  uint64
	Unhelpful    bool
	BusyTime     time.Duration
	PendingBytes uint64
}

// MeasureLane reuses completed content measurements; it never generates probes.
// Retire only an idle, sampled route with a demonstrably faster alternative.
func (s *Set) MeasureLane(identity Identity) Measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity {
		return Measurement{}
	}
	result := Measurement{UsefulBytes: state.usefulBytes, BusyTime: state.performance.BusyTime(s.now()), PendingBytes: state.performance.PendingBytes}
	lastUseful := state.lastUsefulAt
	if lastUseful.IsZero() {
		lastUseful = state.admittedAt
	}
	if state.inflight != 0 || !state.performance.HasSuccessfulSample ||
		s.now().Sub(lastUseful) < UnhelpfulLaneWindow {
		return result
	}
	for _, other := range s.lanes {
		if other != state && !other.retired && s.contentSuspensions[other.identity.ID] == nil &&
			other.performance.HasSuccessfulSample && other.performance.BytesPerSecond > state.performance.BytesPerSecond*UnhelpfulLaneRateRatio &&
			!other.lastUsefulAt.IsZero() && s.now().Sub(other.lastUsefulAt) < UnhelpfulLaneWindow {
			result.Unhelpful = true
			break
		}
	}
	return result
}

// EstimateQueuedContent exposes scheduling evidence, not route permission. Control
// requests remain usable while a lane is suspended for content-only policy.
func (s *Set) EstimateQueuedContent(identity Identity) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity || state.retired {
		return 0
	}
	return state.performance.EstimateQueue()
}
