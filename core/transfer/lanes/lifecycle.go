package lanes

// Stop closes admission and cancels attempts without waiting. BlockSource
// callbacks use it when a lane-local failure must terminate the whole set.
func (s *Set) Stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, state := range s.lanes {
			if state.settlement != nil {
				// Stop is cancellation-only. Holding the final incarnation keeps
				// summaries behind Close's attempt join while allowing replacement
				// and explicit removal to settle independently.
				state.settlementHolds++
				s.finalSettlements = append(s.finalSettlements, state)
			}
			s.retireLaneLocked(state)
		}
		clear(s.lanes)
		clear(s.contentSuspensions)
		s.notifyLaneAvailabilityLocked()
	}
	s.mu.Unlock()
	s.stop()
}

// Close is the external ownership boundary. Attempt callbacks call Stop because
// synchronously joining their own attempt would prevent its deferred completion.
func (s *Set) Close() {
	s.mu.Lock()
	if s.closeStarted {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return
	}
	s.closeStarted = true
	s.mu.Unlock()

	s.Stop()
	s.fetches.Wait()
	// A winner may return without waiting for its hedges, but lane ownership
	// cannot end until cancellation has brought every admitted attempt home.
	s.attempts.Wait()
	s.publications.Wait()
	for _, settlement := range s.finalizeLaneSettlements() {
		s.settlementProducer.TryPublish(settlement)
	}
	s.settlementProducer.Complete()
	close(s.closeDone)
}
