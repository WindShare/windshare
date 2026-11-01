package receive

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session"
)

type cliReceiverPeerAttempt struct {
	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	err       error
	outcome   receiverPeerMonitorOutcome
	lane      session.LaneIdentity
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
func (attempt *cliReceiverPeerAttempt) Lane() (session.LaneIdentity, bool) {
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

func TestReceiverConnectivityKeepsAdmissionOrderingAndPolicy(t *testing.T) {
	admissionErr := errors.New("relay admission failed")
	for _, test := range []struct {
		name           string
		policy         ConnectivityPolicy
		missingPeer    bool
		admissionError error
		wantEvents     []string
		wantError      error
	}{
		{"auto", ConnectivityAuto, false, nil, []string{"relay", "peer"}, nil},
		{"relay only", ConnectivityRelayOnly, false, nil, []string{"relay"}, nil},
		{"peer only", ConnectivityP2POnly, false, nil, []string{"peer"}, nil},
		{"missing required peer", ConnectivityP2POnly, true, nil, []string{"peer"}, errReceiverP2PPathUnavailable},
		{"auto admission failure", ConnectivityAuto, false, admissionErr, []string{"relay"}, admissionErr},
		{"relay admission failure", ConnectivityRelayOnly, false, admissionErr, []string{"relay"}, admissionErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := test.policy.receiverPlan()
			if err != nil {
				t.Fatal(err)
			}
			var events []string
			peer := &activeReceiverPeer{}
			got, err := beginReceiverConnectivity(plan, func() *activeReceiverPeer {
				events = append(events, "peer")
				if test.missingPeer {
					return nil
				}
				return peer
			}, func() error {
				events = append(events, "relay")
				return test.admissionError
			})
			if !errors.Is(err, test.wantError) || !slices.Equal(events, test.wantEvents) {
				t.Fatalf("events=%v err=%v", events, err)
			}
			if err == nil && test.policy != ConnectivityRelayOnly && got != peer {
				t.Fatal("connectivity lost its peer owner")
			}
		})
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
