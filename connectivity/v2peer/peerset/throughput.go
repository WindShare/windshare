package peerset

import (
	"encoding/hex"
	"slices"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/throughput"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/peerslot"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

// The controller borrows scheduling observations; it never creates private
// work queues or retries an optional path with the essential recovery budget.
func (s *ReceiverSession) runContentControl(lanes *transferlanes.Set) {
	var controller throughput.Controller
	for {
		timer := s.owner.config.Clock.NewTimer(throughput.SampleInterval)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		s.recoverContentDemand(lanes)
		demand := lanes.SchedulingDemand()
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		paths, targets, pending, total := s.throughputPaths(lanes)
		if s.config.Demand != ContentDemand {
			demand.NetworkWaitingBytes = 0
			demand.UnfinishedBytes = 0
		}
		observation := throughput.Observation{
			InFlightBytes: demand.InFlightBytes, QueuedBytes: demand.QueuedBytes,
			UnfinishedBytes: demand.UnfinishedBytes, CapacityWaitingBytes: demand.CapacityWaitingBytes,
			NetworkWaitingBytes: demand.NetworkWaitingBytes, UsefulBytes: demand.UsefulBytes,
			OutputBytes: demand.OutputBytes, RemainingBytes: demand.RemainingBytes, RemainingKnown: demand.RemainingKnown,
			Paths: paths, Pending: pending,
		}
		decision := controller.Step(s.owner.config.Clock.Now(), observation, peerslot.Count-1)
		var retire *Path
		target := targets[decision.Path]
		identity := transferlanes.Identity{ID: decision.Path.LaneID, Epoch: decision.Path.LaneEpoch}
		switch decision.Action {
		case throughput.Explore:
			if total < peerslot.Count-1 {
				config := s.config
				config.Purpose, config.Demand = v2signal.ThroughputExploration, ContentDemand
				if path, err := s.owner.openDirect(s.ctx, config); err == nil {
					s.watchPath(path)
				}
			}
		case throughput.Sample:
			if target != nil {
				lanes.SampleLane(identity)
			}
		case throughput.Retire, throughput.Keep, throughput.Abandon:
			lanes.StopSampling(identity)
			if decision.Action == throughput.Retire && target != nil && target.config.Purpose == v2signal.ThroughputExploration {
				retire = target
			}
		}
		if decision.Action != throughput.Hold {
			s.direct.config.Native.ObserveParallelism(nativepeer.Subject{ProtocolSessionID: s.direct.key.session, PeerPathID: s.direct.key.path}, decision, observation)
		}
		s.mu.Unlock()
		if retire != nil {
			retireThroughputGeneration(retire, identity)
		}
	}
}

func (s *ReceiverSession) throughputPaths(lanes *transferlanes.Set) ([]throughput.PathObservation, map[throughput.PathIdentity]*Path, bool, int) {
	s.owner.mu.Lock()
	ordered := make([]*Path, 0, len(s.owner.paths))
	for _, path := range s.owner.paths {
		ordered = append(ordered, path)
	}
	s.owner.mu.Unlock()
	slices.SortFunc(ordered, func(a, b *Path) int {
		if a.sequence < b.sequence {
			return -1
		}
		if a.sequence > b.sequence {
			return 1
		}
		return 0
	})
	observations := make([]throughput.PathObservation, 0, len(ordered))
	targets := make(map[throughput.PathIdentity]*Path)
	pending := false
	for _, path := range ordered {
		if _, relayed := path.config.ICE.Relay(); relayed {
			continue
		}
		lane, admitted := path.Lane()
		if !admitted {
			pending = true
			continue
		}
		work, live := lanes.SamplingEvidence(transferlanes.Identity{ID: lane.ID, Epoch: lane.Epoch})
		if !live {
			pending = true
			continue
		}
		identity := throughput.PathIdentity{PathID: hex.EncodeToString(path.key.path[:]), LaneID: lane.ID, LaneEpoch: lane.Epoch}
		observations = append(observations, throughput.PathObservation{Identity: identity, Optional: path.config.Purpose == v2signal.ThroughputExploration, Work: work})
		targets[identity] = path
	}
	return observations, targets, pending, len(ordered)
}

// Retirement must not close a replacement admitted after the policy snapshot.
func retireThroughputGeneration(path *Path, identity transferlanes.Identity) {
	path.mu.Lock()
	if path.lane.ID != identity.ID || path.lane.Epoch != identity.Epoch {
		path.mu.Unlock()
		return
	}
	path.cancel()
	path.mu.Unlock()
	<-path.done
}

func (s *ReceiverSession) watchPath(path *Path) {
	s.work.Go(func() {
		ready := path.Ready()
		for {
			select {
			case <-ready:
				s.admitted(path)
				ready = nil
			case <-path.Done():
				s.pathFinished(path.Result())
				return
			}
		}
	})
}

func (s *ReceiverSession) pathFinished(result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return
	}
	if result.Scope != protocolsession.PeerFailureSessionTerminal {
		s.owner.mu.Lock()
		recoverable := len(s.owner.paths) > 0 || s.turn != nil
		s.owner.mu.Unlock()
		if recoverable {
			return
		}
	}
	s.result = result
	s.cancel()
}
