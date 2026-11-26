package sessioncore

import (
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
)

// State belongs to the transport. It can reenter this runtime, and the receive
// pump can lag a physical close. Observe it outside the registry lock and
// verify the same epoch again before transferring ownership.
func (lanes *runtimeLanes) transportOpen(identity session.LaneIdentity) bool {
	candidate, err := lanes.selectLane(&identity)
	return err == nil && candidate.channel.State() == framechannel.Open
}

func (lanes *runtimeLanes) observeHandoff(old session.LaneIdentity, successor *session.LaneIdentity) (oldClosed bool, open []session.LaneIdentity) {
	if successor != nil {
		if lanes.transportOpen(*successor) {
			open = append(open, *successor)
		}
		return oldClosed, open
	}
	for _, candidate := range lanes.snapshot() {
		state := candidate.channel.State()
		if candidate.identity == old {
			oldClosed = state == framechannel.Closed
		} else if state == framechannel.Open {
			open = append(open, candidate.identity)
		}
	}
	return oldClosed, open
}

func (lanes *runtimeLanes) retireIfCovered(identity session.LaneIdentity, successor *session.LaneIdentity) bool {
	if !identity.Valid(true) || successor != nil && *successor == identity {
		return false
	}
	oldClosed, open := lanes.observeHandoff(identity, successor)
	lanes.mu.Lock()
	covered := false
	for _, candidate := range open {
		if _, err := lanes.selectLaneLocked(&candidate, nil); err == nil {
			covered = true
			break
		}
	}
	if successor != nil && !covered {
		lanes.mu.Unlock()
		return false
	}
	lane := lanes.active[identity.ID]
	if lane == nil || lane.identity != identity || lane.closing {
		lanes.mu.Unlock()
		return true
	}
	// Terminal fanout owns the remaining lifetime of active lanes; replacement
	// handoff must never detach a lane actively delivering or awaiting SessionTerminal.
	if (lane.writer != nil && lane.writer.DeliveringTerminal()) ||
		(lanes.terminalFanout && lane.activeLocked()) {
		lanes.mu.Unlock()
		return false
	}
	if lane.usableLocked() && !oldClosed && !covered {
		lanes.mu.Unlock()
		return false
	}
	lanes.detachAndUnlock(lane)
	return true
}
