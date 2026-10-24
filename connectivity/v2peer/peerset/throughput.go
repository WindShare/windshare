package peerset

import (
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/throughput"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/peerslot"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

// The controller borrows scheduling observations; it never creates private
// work queues or retries an optional path with the essential recovery budget.
func (s *ReceiverSession) runThroughput(lanes *transferlanes.Set) {
	var controller throughput.Controller
	for {
		timer := s.owner.config.Clock.NewTimer(throughput.SampleInterval)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		demand := lanes.SchedulingDemand()
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		paths, pending, optional, totalPaths := s.throughputPaths()
		if s.config.Demand != ContentDemand {
			demand.QueuedBytes = 0
			demand.NetworkWaitingBytes = 0
			demand.UnfinishedBytes = 0
		}
		observation := throughput.Observation{
			InFlightBytes: demand.InFlightBytes,
			QueuedBytes:   demand.QueuedBytes, UnfinishedBytes: demand.UnfinishedBytes, CapacityWaitingBytes: demand.CapacityWaitingBytes,
			NetworkWaitingBytes: demand.NetworkWaitingBytes,
			UsefulBytes:         demand.UsefulBytes, OutputBytes: demand.OutputBytes, RemainingBytes: demand.RemainingBytes, RemainingKnown: demand.RemainingKnown,
			Paths: paths, OptionalPaths: len(optional), Pending: pending,
		}
		decision := controller.Step(s.owner.config.Clock.Now(), observation, peerslot.Count-1)
		var retire *Path
		if decision.Action == throughput.Explore && totalPaths < peerslot.Count-1 {
			config := s.config
			config.Purpose, config.Demand = v2signal.ThroughputExploration, ContentDemand
			path, err := s.owner.openDirect(s.ctx, config)
			if err == nil {
				s.watchPath(path)
			}
		} else if decision.Action == throughput.Retire && len(optional) > 0 {
			retire = newestOptionalPath(optional)
		}
		if decision.Action != throughput.Hold {
			s.direct.config.Native.ObserveParallelism(nativepeer.Subject{ProtocolSessionID: s.direct.key.session, PeerPathID: s.direct.key.path}, decision, observation)
		}
		s.mu.Unlock()
		if retire != nil {
			_ = retire.Close()
		}
	}
}

func (s *ReceiverSession) throughputPaths() (live int, pending bool, optional []*Path, total int) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	for _, path := range s.owner.paths {
		if _, relayed := path.config.ICE.Relay(); relayed {
			continue
		}
		if _, admitted := path.Lane(); admitted {
			live++
		} else {
			pending = true
		}
		if path.config.Purpose == v2signal.ThroughputExploration {
			optional = append(optional, path)
		}
	}
	return live, pending, optional, len(s.owner.paths)
}

func newestOptionalPath(paths []*Path) *Path {
	// Map iteration must not decide which established connection survives an
	// inconclusive trial; the newest expansion owns the experiment.
	newest := paths[0]
	for _, path := range paths[1:] {
		if path.sequence > newest.sequence {
			newest = path
		}
	}
	return newest
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
