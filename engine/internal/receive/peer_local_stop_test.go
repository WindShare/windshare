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

// A caller cancellation reaches the peer directly, usually before settlement
// records a stop reason; the termination it causes must still name the caller.
func TestReceiverPeerTerminationAfterCallerStopNamesCaller(t *testing.T) {
	var mu sync.Mutex
	var events []task.Event
	observation := newObservation(task.Control{Now: time.Now, Emit: func(event task.Event) bool {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		return true
	}}, true, nil)
	source := &testReceiverObservationSource{
		terminal:   make(chan v2peer.ReceiverTerminationTrace, 1),
		diagnostic: make(chan v2peer.PeerDiagnosticObservation),
	}
	worker := &runner{receiverPeerFactory: func() (receiverPeerStarter, error) {
		return testObservedReceiverStarter{source}, nil
	}}
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := &receiverLocalStop{}
	worker.startReceiverPeer(ctx, nil, observation, nil, stop, receiverPeerPreferred)
	cancel(task.ErrCancelled)
	source.terminal <- v2peer.ReceiverTerminationTrace{}
	observation.complete()

	mu.Lock()
	defer mu.Unlock()
	var terminated []PeerTerminated
	for _, event := range events {
		if value, ok := event.(PeerTerminated); ok {
			terminated = append(terminated, value)
		}
	}
	if len(terminated) != 1 || terminated[0].LocalStop != ReceiverLocalStopCaller {
		t.Fatalf("peer terminations=%+v", terminated)
	}
	// Teardown started by the caller stop does not replace its reason.
	stop.record(ReceiverLocalStopRuntimeSessionFailure)
	if stop.snapshot() != ReceiverLocalStopCaller {
		t.Fatalf("stop reason after caller teardown=%v", stop.snapshot())
	}
	// A reason recorded before the caller stop stays authoritative.
	completed := &receiverLocalStop{}
	completedCtx, completedCancel := context.WithCancel(context.Background())
	completed.bindCaller(completedCtx)
	completed.record(ReceiverLocalStopNormalCompletion)
	completedCancel()
	if completed.snapshot() != ReceiverLocalStopNormalCompletion {
		t.Fatalf("stop reason after completion=%v", completed.snapshot())
	}
}
