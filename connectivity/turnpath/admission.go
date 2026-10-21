package turnpath

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/windshare/windshare/core/session/turncontrol"
)

// Admission is the revocable authority of one authenticated lane incarnation.
// A grant can outlive several attempts; closing an old admission cannot revoke
// the next attempt's authority.
type Admission struct {
	sender   *Sender
	path     [16]byte
	lane     turncontrol.Lane
	done     <-chan struct{}
	lifetime context.Context
}

func (s *Sender) Admit(ctx context.Context, path [16]byte, lane turncontrol.Lane, done <-chan struct{}) *Admission {
	if ctx == nil || ctx.Err() != nil || !lane.Valid() || done == nil {
		return nil
	}
	s.mu.Lock()
	if s.controls.check(ctx) != nil {
		s.mu.Unlock()
		return nil
	}
	value := s.paths[path]
	if value == nil || lane.Epoch <= value.admissionEpoch || signalClosed(done) {
		s.mu.Unlock()
		return nil
	}
	admission := &Admission{sender: s, path: path, lane: lane, done: done, lifetime: ctx}
	value.admission, value.admissionEpoch = admission, lane.Epoch
	s.mu.Unlock()
	admission.observe("admitted")
	s.wake()
	return admission
}

func (a *Admission) Close() {
	if a == nil {
		return
	}
	a.sender.mu.Lock()
	value := a.sender.paths[a.path]
	revoked := value != nil && value.admission == a
	if revoked {
		value.admission = nil
	}
	a.sender.mu.Unlock()
	if revoked {
		a.observe("revoked")
		a.sender.wake()
	}
}

func (a *Admission) live() bool {
	return !signalClosed(a.done) && a.lifetime.Err() == nil
}

func (a *Admission) observe(state string) {
	slog.Debug("TURN lane admission", "session_id", fmt.Sprintf("%x", a.sender.config.SessionID),
		"path_id", fmt.Sprintf("%x", a.path), "lane_id", a.lane.ID, "lane_epoch", a.lane.Epoch, "state", state)
}

func signalClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (s *Sender) wake() {
	select {
	case s.reconcileWake <- struct{}{}:
	default:
	}
}
