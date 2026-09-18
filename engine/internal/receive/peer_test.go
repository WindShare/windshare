package receive

import (
	"errors"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"sync"
	"testing"
	"time"
)

type cliReceiverPeerAttempt struct {
	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	err       error
	outcome   receiverPeerMonitorOutcome
	lane      sessionruntime.LaneIdentity
}

func newCLIReceiverPeerAttempt() *cliReceiverPeerAttempt {
	return &cliReceiverPeerAttempt{ready: make(chan struct{}), done: make(chan struct{})}
}

func (attempt *cliReceiverPeerAttempt) Ready() <-chan struct{} { return attempt.ready }
func (attempt *cliReceiverPeerAttempt) Done() <-chan struct{}  { return attempt.done }
func (attempt *cliReceiverPeerAttempt) Err() error             { return attempt.err }
func (attempt *cliReceiverPeerAttempt) Outcome() receiverPeerMonitorOutcome {
	return attempt.outcome
}
func (attempt *cliReceiverPeerAttempt) Lane() (sessionruntime.LaneIdentity, bool) {
	return attempt.lane, attempt.lane.ID != 0 && attempt.lane.Epoch != 0
}
func (attempt *cliReceiverPeerAttempt) Close() error {
	attempt.closeOnce.Do(func() {
		attempt.outcome = receiverPeerMonitorOutcome{
			disposition: receiverPeerLocalStop,
		}
		close(attempt.done)
	})
	return nil
}
func (attempt *cliReceiverPeerAttempt) finish(err error) {
	attempt.finishOutcome(receiverPeerMonitorOutcome{
		disposition:   receiverPeerFallbackAllowed,
		retainedCause: err,
	})
}

func (attempt *cliReceiverPeerAttempt) finishOutcome(outcome receiverPeerMonitorOutcome) {
	attempt.closeOnce.Do(func() {
		attempt.err = outcome.retainedCause
		attempt.outcome = outcome
		close(attempt.done)
	})
}

func TestReceiverPeerStartsBeforeBlockingSelectionPlanning(t *testing.T) {
	plan, err := ConnectivityAuto.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	peerStarted := make(chan struct{})
	selectionEntered := make(chan struct{})
	releaseSelection := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = beginReceiverPlanning(
			plan,
			func() *activeReceiverPeer {
				close(peerStarted)
				return nil
			},
			func() error {
				// Auto admits its authenticated relay before peer setup and selection.
				return nil
			},
			func() (transfer.SelectionRules, error) {
				close(selectionEntered)
				<-releaseSelection
				return transfer.SelectionRules{}, nil
			},
		)
	}()
	select {
	case <-selectionEntered:
	case <-time.After(time.Second):
		t.Fatal("selection planning did not begin")
	}
	select {
	case <-peerStarted:
	default:
		t.Fatal("blocking selection planning began before the peer race")
	}
	close(releaseSelection)
	<-done
}

func TestReceiverRelayOnlyPlanningNeverCreatesPeerAttempt(t *testing.T) {
	plan, err := ConnectivityRelayOnly.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	peerStarts := 0
	relayResumes := 0
	_, _, err = beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer {
			peerStarts++
			return nil
		},
		func() error {
			relayResumes++
			return nil
		},
		func() (transfer.SelectionRules, error) { return transfer.SelectionRules{}, nil },
	)
	if err != nil || peerStarts != 0 || relayResumes != 1 {
		t.Fatalf("relay-only planning: err=%v peer_starts=%d relay_resumes=%d", err, peerStarts, relayResumes)
	}
}

func TestReceiverP2POnlyPlanningRequiresPeerAndNeverAdmitsRelay(t *testing.T) {
	plan, err := ConnectivityP2POnly.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	relayAdmissions := 0
	peer := &activeReceiverPeer{}
	got, _, err := beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer { return peer },
		func() error {
			relayAdmissions++
			return nil
		},
		func() (transfer.SelectionRules, error) { return transfer.SelectionRules{}, nil },
	)
	if err != nil || got != peer || relayAdmissions != 0 {
		t.Fatalf("p2p-only planning: peer=%p err=%v relay_admissions=%d", got, err, relayAdmissions)
	}

	_, _, err = beginReceiverPlanning(
		plan,
		func() *activeReceiverPeer { return nil },
		func() error {
			t.Fatal("p2p-only planning admitted relay content")
			return nil
		},
		func() (transfer.SelectionRules, error) {
			t.Fatal("selection resolved without the required direct peer")
			return transfer.SelectionRules{}, nil
		},
	)
	if !errors.Is(err, errReceiverP2PPathUnavailable) {
		t.Fatalf("missing p2p-only peer error=%v", err)
	}
}

func TestConnectivityPolicyRejectsUnknownValues(t *testing.T) {
	for name, want := range map[string]ConnectivityPolicy{
		"auto": ConnectivityAuto, "relay-only": ConnectivityRelayOnly, "p2p-only": ConnectivityP2POnly,
	} {
		got, err := ParseConnectivityPolicy(name)
		if err != nil || got != want || got.String() != name {
			t.Fatalf("parse %q = %v, %v", name, got, err)
		}
	}
	if _, err := ParseConnectivityPolicy("relay"); !errors.Is(err, ErrInvalidConnectivityPolicy) {
		t.Fatalf("unknown policy error = %v", err)
	}
}
