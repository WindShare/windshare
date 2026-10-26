package sessioncore

import (
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"time"
)

// Selection and reservation share the registry lock, so concurrent callers see
// each other's pending work and no request can reserve an already detached lane.
func (lanes *runtimeLanes) selectRequestLane(
	preferred *sessioncontract.LaneIdentity, kind protocolsession.MessageKind,
) (selectedLane, *requestlane.Reservation, requestlane.Estimate, error) {
	if preferred != nil || !requestlane.Managed(kind) {
		selected, err := lanes.selectLane(preferred)
		return selected, nil, requestlane.Estimate{}, err
	}
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if lanes.stopping || lanes.runtime.ctx.Err() != nil {
		return selectedLane{}, nil, requestlane.Estimate{}, sessioncontract.ErrRuntimeClosed
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
