package lanes

import "github.com/windshare/windshare/core/transfer/lanescheduling"

// SampleLane grants a bounded opportunity, not a fixed traffic share. Allocation
// still owns route eligibility, output-frontier priority and physical capacity.
func (s *Set) SampleLane(identity Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity || state.retired {
		return
	}
	for _, lane := range s.lanes {
		lane.sampling.Stop()
	}
	state.sampling.Start(s.now())
	s.notifyAdmissionChangedLocked()
}

func (s *Set) StopSampling(identity Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lanes[identity.ID]
	if state != nil && state.identity == identity {
		state.sampling.Stop()
	}
}

func (s *Set) SamplingEvidence(identity Identity) (lanescheduling.SamplingEvidence, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.lanes[identity.ID]
	if state == nil || state.identity != identity || state.retired {
		return lanescheduling.SamplingEvidence{}, false
	}
	return state.sampling.Snapshot(s.now()), true
}
