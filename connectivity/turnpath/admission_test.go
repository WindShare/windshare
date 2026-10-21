package turnpath

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

func replacementSender(t *testing.T) (*Sender, *delegation, *delegation) {
	t.Helper()
	now := time.Unix(1700000000, 0)
	accounts := &accountFixture{now: func() time.Time { return now }}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now,
		Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	issueNext(t, s)
	old := s.services[0].pending
	lane := turncontrol.Lane{ID: 1, Epoch: 1}
	s.Admit(t.Context(), old.grant.PathID, lane, make(chan struct{}))
	s.apply(t.Context(), turncontrol.Ready{Authority: old.grant.Authority, PathID: old.grant.PathID, Lane: lane})
	now = now.Add(8 * time.Minute)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: time.Minute})
	issueNext(t, s)
	next := s.services[0].pending
	if next == nil || next.grant.Replaces != old.grant.PathID {
		t.Fatal("fixture did not create a renewal with a live predecessor")
	}
	return s, old, next
}

func TestTerminatedReplacementCannotRetireHealthyPredecessor(t *testing.T) {
	for _, terminal := range []string{"channel", "attempt", "revocation"} {
		t.Run(terminal, func(t *testing.T) {
			s, old, next := replacementSender(t)
			var retired [][16]byte
			s.config.Retire = func(path [16]byte) { retired = append(retired, path) }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			lane := turncontrol.Lane{ID: 2, Epoch: 2}
			admission := s.Admit(ctx, next.grant.PathID, lane, done)
			if admission == nil {
				t.Fatal("live replacement was not admitted")
			}
			switch terminal {
			case "channel":
				close(done)
			case "attempt":
				cancel()
			case "revocation":
				admission.Close()
			}
			// A Ready arriving on any surviving control lane must not resurrect
			// the already-ended connection, even before its cleanup callback runs.
			if err := s.Handle(t.Context(), controlBody(t, turncontrol.Ready{
				Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: lane,
			})); err != nil {
				t.Fatal(err)
			}
			drainControls(t, s.controls, s.apply)
			s.maintain(t.Context())
			if s.services[0].current != old || len(retired) != 0 {
				t.Fatal("late readiness retired the healthy predecessor")
			}
		})
	}
}

func TestAdmissionAndReadinessBelongToTheSameLaneIncarnation(t *testing.T) {
	s, old, next := replacementSender(t)
	oldLane := turncontrol.Lane{ID: 2, Epoch: 2}
	newLane := turncontrol.Lane{ID: 2, Epoch: 3}
	first := s.Admit(t.Context(), next.grant.PathID, oldLane, make(chan struct{}))
	second := s.Admit(t.Context(), next.grant.PathID, newLane, make(chan struct{}))
	if first == nil || second == nil {
		t.Fatal("fixture admissions failed")
	}
	first.Close()
	first.Close()
	if admission := s.Admit(t.Context(), next.grant.PathID, oldLane, make(chan struct{})); admission != nil {
		t.Fatal("old attempt reclaimed a newer lane incarnation")
	}
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: oldLane})
	if s.services[0].current != old {
		t.Fatal("old readiness authorized a new attempt")
	}
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: newLane})
	if s.services[0].current != next {
		t.Fatal("old cleanup revoked the replacement admission")
	}
	if _, known, err := s.Lookup(old.grant.PathID); !known || err == nil {
		t.Fatal("successful replacement did not retire its predecessor")
	}
}

func TestReadinessBeforeAdmissionCannotBeRolledBackByReplay(t *testing.T) {
	s, _, next := replacementSender(t)
	lane := turncontrol.Lane{ID: 2, Epoch: 3}
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: lane})
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID,
		Lane: turncontrol.Lane{ID: 2, Epoch: 2}})
	s.Admit(t.Context(), next.grant.PathID, lane, make(chan struct{}))
	s.maintain(t.Context())
	if s.services[0].current != next {
		t.Fatal("replay erased readiness for the incoming admission")
	}
}

func TestStoppedSenderCannotReauthorizeDelegatedPaths(t *testing.T) {
	s, _, next := replacementSender(t)
	s.close()
	if _, known, err := s.Lookup(next.grant.PathID); !known || err == nil {
		t.Fatal("shutdown allowed a delegated path to fall through to direct preparation")
	}
	if s.Admit(t.Context(), next.grant.PathID, turncontrol.Lane{ID: 2, Epoch: 2}, make(chan struct{})) != nil {
		t.Fatal("stopped sender accepted admission")
	}
}

func TestEndedOrUnknownAdmissionCannotAcquireAuthority(t *testing.T) {
	s, _, next := replacementSender(t)
	lane := turncontrol.Lane{ID: 2, Epoch: 2}
	done := make(chan struct{})
	close(done)
	if s.Admit(t.Context(), next.grant.PathID, lane, done) != nil ||
		s.Admit(t.Context(), [16]byte{99}, lane, make(chan struct{})) != nil ||
		s.Admit(t.Context(), next.grant.PathID, turncontrol.Lane{}, make(chan struct{})) != nil ||
		s.Admit(t.Context(), next.grant.PathID, lane, nil) != nil {
		t.Fatal("invalid or ended admission acquired promotion authority")
	}
}
