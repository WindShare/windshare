package receivercontinuation

import transferlanes "github.com/windshare/windshare/core/transfer/lanes"

type remainingObservation struct {
	snapshot func() transferlanes.RemainingWork
}

// A transfer and its output survive protocol replacement. Keep its demand on
// the active generation, otherwise a bounded read window hides the rest of the job.
func (s *Session) ObserveRemainingWork(snapshot func() transferlanes.RemainingWork) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return func() {}
	}
	observer := &remainingObservation{snapshot: snapshot}
	if s.remaining == nil {
		s.remaining = make(map[*remainingObservation]func())
	}
	s.remaining[observer] = s.current.LaneSet().ObserveRemainingWork(snapshot)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if release, exists := s.remaining[observer]; exists {
			release()
			delete(s.remaining, observer)
		}
	}
}

func (s *Session) RecordOutput(bytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() == nil {
		// Output pressure belongs to the continuing job, including a range that
		// retained authenticated bytes while its network generation changed.
		s.current.LaneSet().RecordOutput(bytes)
	}
}
