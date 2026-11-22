package receive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/engine/internal/task"
)

type testObservedReceiverStarter struct {
	*testReceiverObservationSource
}

func (testObservedReceiverStarter) Start(context.Context, *receiversession.Runtime) (receiverPeerAttempt, error) {
	return nil, errors.New("no attempt")
}

type localStopProbe struct {
	mu          sync.Mutex
	events      []task.Event
	observation getObservation
	source      *testReceiverObservationSource
	worker      *runner
}

func newLocalStopProbe(caller context.Context) *localStopProbe {
	probe := &localStopProbe{source: &testReceiverObservationSource{
		terminal:   make(chan v2peer.ReceiverTerminationTrace, 1),
		diagnostic: make(chan v2peer.PeerDiagnosticObservation),
	}}
	probe.observation = newObservation(task.Control{Now: time.Now, Emit: func(event task.Event) bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.events = append(probe.events, event)
		return true
	}}, true, nil)
	probe.worker = &runner{caller: caller, receiverPeerFactory: func() (receiverPeerStarter, error) {
		return testObservedReceiverStarter{probe.source}, nil
	}}
	return probe
}

// terminate publishes one peer termination and returns the stop reason it carried.
func (probe *localStopProbe) terminate(t *testing.T) ReceiverLocalStopReason {
	t.Helper()
	probe.source.terminal <- v2peer.ReceiverTerminationTrace{}
	probe.observation.complete()
	probe.mu.Lock()
	defer probe.mu.Unlock()
	var terminated []PeerTerminated
	for _, event := range probe.events {
		if value, ok := event.(PeerTerminated); ok {
			terminated = append(terminated, value)
		}
	}
	if len(terminated) != 1 {
		t.Fatalf("peer terminations=%+v", terminated)
	}
	return terminated[0].LocalStop
}

// A task stop reaches the peer directly, usually before settlement records a
// stop reason; the termination it causes must still name the caller.
func TestReceiverPeerTerminationAfterCallerStopNamesCaller(t *testing.T) {
	taskCtx, stopTask := context.WithCancelCause(context.Background())
	probe := newLocalStopProbe(taskCtx)
	stop := &receiverLocalStop{}
	probe.worker.startReceiverPeer(taskCtx, nil, probe.observation, nil, stop, receiverPeerPreferred)
	stopTask(task.ErrCancelled)
	if reason := probe.terminate(t); reason != ReceiverLocalStopCaller {
		t.Fatalf("termination after task stop=%v", reason)
	}
	// Teardown started by the task stop does not replace its reason.
	stop.record(ReceiverLocalStopRuntimeSessionFailure)
	if stop.snapshot() != ReceiverLocalStopCaller {
		t.Fatalf("stop reason after caller teardown=%v", stop.snapshot())
	}
}

// After a session replacement the peer runs under the continuation session's
// context, which ends on every normal finish before settlement records why.
// Only the task stopping is a caller stop.
func TestReceiverPeerReplacedGenerationKeepsSettlementReason(t *testing.T) {
	for _, reason := range []ReceiverLocalStopReason{
		ReceiverLocalStopNormalCompletion,
		ReceiverLocalStopRuntimeSessionFailure,
		ReceiverLocalStopOutputAdmission,
	} {
		t.Run(reason.DiagnosticName(), func(t *testing.T) {
			taskCtx, stopTask := context.WithCancelCause(context.Background())
			defer stopTask(nil)
			sessionCtx, closeSession := context.WithCancel(taskCtx)
			probe := newLocalStopProbe(taskCtx)
			stop := &receiverLocalStop{}
			probe.worker.startReceiverPeer(sessionCtx, nil, probe.observation, nil, stop, receiverPeerPreferred)
			closeSession()
			if got := stop.snapshot(); got != ReceiverLocalStopNone {
				t.Fatalf("session close alone=%v", got)
			}
			stop.record(reason)
			if got := probe.terminate(t); got != reason {
				t.Fatalf("termination reason=%v want %v", got, reason)
			}
		})
	}
}

// A reason recorded before the task stops stays authoritative.
func TestReceiverLocalStopKeepsReasonRecordedBeforeTaskStop(t *testing.T) {
	taskCtx, stopTask := context.WithCancelCause(context.Background())
	stop := &receiverLocalStop{}
	stop.bindCaller(taskCtx)
	stop.record(ReceiverLocalStopNormalCompletion)
	stopTask(task.ErrCancelled)
	if stop.snapshot() != ReceiverLocalStopNormalCompletion {
		t.Fatalf("stop reason after completion=%v", stop.snapshot())
	}
}
