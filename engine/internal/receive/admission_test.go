package receive

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type fakeReceiverContentSuspension struct {
	mu          sync.Mutex
	resumeCount int
	resumeError error
	resumeEvent chan struct{}
	resumeGate  <-chan struct{}
	resumeOnce  sync.Once
}

func newFakeReceiverContentSuspension() *fakeReceiverContentSuspension {
	return &fakeReceiverContentSuspension{resumeEvent: make(chan struct{})}
}

func (suspension *fakeReceiverContentSuspension) Resume() error {
	suspension.mu.Lock()
	suspension.resumeCount++
	err := suspension.resumeError
	gate := suspension.resumeGate
	suspension.mu.Unlock()
	suspension.resumeOnce.Do(func() { close(suspension.resumeEvent) })
	if gate != nil {
		<-gate
	}
	return err
}

func (suspension *fakeReceiverContentSuspension) count() int {
	suspension.mu.Lock()
	defer suspension.mu.Unlock()
	return suspension.resumeCount
}

type inertReceiverBlockLane struct{}

func (inertReceiverBlockLane) FetchBlock(
	context.Context,
	transferlanes.Demand, func(uint64),
) (records.BlockRecord, error) {
	return records.BlockRecord{}, errors.New("admission test must not fetch content")
}

func TestReceiverContentAdmissionValidatesInputs(t *testing.T) {
	if _, err := newReceiverContentAdmission(receiverConnectivityPlan{}, newFakeReceiverContentSuspension(), receiverAdmissionHooks{}); !errors.Is(err, ErrInvalidReceiverAdmission) {
		t.Fatalf("invalid plan err=%v", err)
	}
	autoPlan, _ := ConnectivityAuto.receiverPlan()
	if _, err := newReceiverContentAdmission(autoPlan, nil, receiverAdmissionHooks{}); !errors.Is(err, ErrInvalidReceiverAdmission) {
		t.Fatalf("nil relay suspension err=%v", err)
	}
	admission, err := newReceiverContentAdmission(autoPlan, newFakeReceiverContentSuspension(), receiverAdmissionHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObservePeer(receiverPeerSignal(99)); !errors.Is(err, ErrInvalidReceiverAdmission) {
		t.Fatalf("unknown signal err=%v", err)
	}
	var nilAdmission *receiverContentAdmission
	nilAdmission.Close()
	nilAdmission.Wait()
	if nilAdmission.Err() != nil {
		t.Fatalf("nil admission err=%v", nilAdmission.Err())
	}
}

func TestRelayContentAdmissionUsesPolicyTriggerAndRespectsClaimGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      ConnectivityPolicy
		wantTrigger receiverAdmissionTrigger
		preClosed   bool
	}{
		{"auto gated", ConnectivityAuto, receiverAdmissionTriggerAuto, false},
		{"relay-only gated", ConnectivityRelayOnly, receiverAdmissionTriggerRelayOnly, false},
		{"auto already ready", ConnectivityAuto, receiverAdmissionTriggerAuto, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := tc.policy.receiverPlan()
			if err != nil {
				t.Fatal(err)
			}
			relay := newFakeReceiverContentSuspension()
			claimGate := make(chan struct{})
			if tc.preClosed {
				close(claimGate)
			}
			var order []string
			var gotTrigger receiverAdmissionTrigger
			var gotDecision receiverAdmissionDecision
			admission, err := newReceiverContentAdmission(plan, relay, receiverAdmissionHooks{
				claimGate: claimGate,
				onClaim: func(trigger receiverAdmissionTrigger) {
					gotTrigger = trigger
					order = append(order, "claim")
				},
				onDecision: func(decision receiverAdmissionDecision) {
					gotDecision = decision
					order = append(order, "decision")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { admission.Close(); admission.Wait() }()
			if err := admission.AdmitRelay(); err != nil {
				t.Fatal(err)
			}
			if !tc.preClosed && relay.count() != 0 {
				t.Fatal("relay resumed before claim gate closed")
			}
			// Peer fallback signals are no-ops once relay admission is scheduled.
			for _, sig := range []receiverPeerSignal{receiverPeerReady, receiverPeerFailed, receiverPeerDetached} {
				if err := admission.ObservePeer(sig); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.preClosed {
				close(claimGate)
			}
			admission.Wait()
			if relay.count() != 1 || gotTrigger != tc.wantTrigger || gotDecision.Trigger != tc.wantTrigger ||
				gotDecision.TerminalOwner != receiverAdmissionTerminalNone || gotDecision.Cause != nil {
				t.Fatalf("count=%d trigger=%q decision=%+v", relay.count(), gotTrigger, gotDecision)
			}
			if len(order) != 2 || order[0] != "claim" || order[1] != "decision" {
				t.Fatalf("order=%v", order)
			}
		})
	}
}

func TestRelayContentAdmissionRevocationPreventsResumeAndLateWarnings(t *testing.T) {
	t.Run("revoked before gate", func(t *testing.T) {
		for _, signal := range []receiverPeerSignal{receiverPeerSessionFatal, receiverPeerRuntimeTerminal} {
			plan, _ := ConnectivityAuto.receiverPlan()
			relay := newFakeReceiverContentSuspension()
			gate := make(chan struct{})
			decided := false
			admission, err := newReceiverContentAdmission(plan, relay, receiverAdmissionHooks{
				claimGate:  gate,
				onDecision: func(receiverAdmissionDecision) { decided = true },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := admission.AdmitRelay(); err != nil {
				t.Fatal(err)
			}
			if err := admission.ObservePeer(signal); err != nil {
				t.Fatal(err)
			}
			close(gate)
			admission.Wait()
			if relay.count() != 0 || decided {
				t.Fatalf("revoked admission resumed=%d decided=%v", relay.count(), decided)
			}
		}
	})

	t.Run("resume failure closes runtime once unless revoked first", func(t *testing.T) {
		plan, _ := ConnectivityAuto.receiverPlan()
		resumeErr := errors.New("lane stale")
		relay := newFakeReceiverContentSuspension()
		relay.resumeError = resumeErr
		observation, events := newTestObservation(t)
		runtime := &testRuntimeCloser{}
		localStop := &receiverLocalStop{}
		admission, err := newReceiverContentAdmission(plan, relay, receiverAdmissionHooks{
			onDecision: func(decision receiverAdmissionDecision) {
				recordReceiverAdmissionDecision(decision, runtime, observation, localStop)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := admission.AdmitRelay(); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(admission.Err(), resumeErr) || runtime.calls.Load() != 1 || localStop.snapshot() != ReceiverLocalStopOutputAdmission {
			t.Fatalf("err=%v closes=%d stop=%v", admission.Err(), runtime.calls.Load(), localStop.snapshot())
		}
		if warnings := warningEvents(*events); len(warnings) != 1 || !errors.Is(warnings[0].(Warning).Cause, resumeErr) {
			t.Fatalf("warnings=%v", warnings)
		}

		// Revoking during an in-flight Resume retains the error without emitting a late warning.
		lateRelay := newFakeReceiverContentSuspension()
		lateRelay.resumeError = resumeErr
		resumeGate := make(chan struct{})
		lateRelay.resumeGate = resumeGate
		claimGate := make(chan struct{})
		lateObs, lateEvents := newTestObservation(t)
		lateRuntime := &testRuntimeCloser{}
		lateAdmission, err := newReceiverContentAdmission(plan, lateRelay, receiverAdmissionHooks{
			claimGate: claimGate,
			onDecision: func(decision receiverAdmissionDecision) {
				recordReceiverAdmissionDecision(decision, lateRuntime, lateObs, &receiverLocalStop{})
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = lateAdmission.AdmitRelay()
		close(claimGate)
		<-lateRelay.resumeEvent
		_ = lateAdmission.ObservePeer(receiverPeerRuntimeTerminal)
		close(resumeGate)
		lateAdmission.Wait()
		if !errors.Is(lateAdmission.Err(), resumeErr) || lateRuntime.calls.Load() != 0 || len(warningEvents(*lateEvents)) != 0 {
			t.Fatalf("late err=%v closes=%d warnings=%v", lateAdmission.Err(), lateRuntime.calls.Load(), warningEvents(*lateEvents))
		}
	})
}

func TestP2POnlyAdmissionEnforcesDirectLossWithoutTouchingRelay(t *testing.T) {
	plan, err := ConnectivityP2POnly.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []receiverPeerSignal{receiverPeerFailed, receiverPeerDetached} {
		observation, events := newTestObservation(t)
		runtime := &testRuntimeCloser{}
		localStop := &receiverLocalStop{}
		admission, err := newReceiverContentAdmission(plan, nil, receiverAdmissionHooks{
			onDecision: func(decision receiverAdmissionDecision) {
				recordReceiverAdmissionDecision(decision, runtime, observation, localStop)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := admission.AdmitRelay(); !errors.Is(err, ErrInvalidReceiverAdmission) {
			t.Fatalf("p2p-only AdmitRelay err=%v", err)
		}
		if err := admission.ObservePeer(receiverPeerReady); err != nil {
			t.Fatal(err)
		}
		if err := admission.ObservePeer(signal); err != nil {
			t.Fatal(err)
		}
		// Duplicate peer loss cannot close runtime twice.
		if err := admission.ObservePeer(signal); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(admission.Err(), errReceiverP2PPathUnavailable) || runtime.calls.Load() != 1 || localStop.snapshot() != ReceiverLocalStopOutputAdmission {
			t.Fatalf("err=%v closes=%d stop=%v", admission.Err(), runtime.calls.Load(), localStop.snapshot())
		}
		if warnings := warningEvents(*events); len(warnings) != 1 || warnings[0].(Warning).Code != FailurePeerStopped {
			t.Fatalf("warnings=%v", warnings)
		}
		admission.Close()
		admission.Wait()
	}

	// Session-fatal or caller close does not misclassify as p2p_unavailable.
	admission, err := newReceiverContentAdmission(plan, nil, receiverAdmissionHooks{})
	if err != nil {
		t.Fatal(err)
	}
	_ = admission.ObservePeer(receiverPeerSessionFatal)
	_ = admission.ObservePeer(receiverPeerFailed)
	if admission.Err() != nil {
		t.Fatalf("fatal session overwritten by p2p error: %v", admission.Err())
	}
}

func TestRelayContentAdmissionSurvivesRelayEpochReplacement(t *testing.T) {
	plan, _ := ConnectivityAuto.receiverPlan()
	lanes, err := transferlanes.New(transferlanes.Config{ProtocolSessionID: protocolsession.ProtocolSessionID{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	initial := transferlanes.Identity{ID: 1, Epoch: 1}
	if err := lanes.Add(initial, transferlanes.RouteRelay, inertReceiverBlockLane{}); err != nil {
		t.Fatal(err)
	}
	relay, err := lanes.SuspendContent(initial)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	admission, err := newReceiverContentAdmission(plan, relay, receiverAdmissionHooks{claimGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { admission.Close(); admission.Wait() }()
	if err := admission.AdmitRelay(); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(transferlanes.Identity{ID: initial.ID, Epoch: initial.Epoch + 1}, transferlanes.RouteRelay, inertReceiverBlockLane{}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	admission.Wait()
	if admission.Err() != nil {
		t.Fatalf("relay replacement resume err=%v", admission.Err())
	}
}
