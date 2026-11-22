package receive

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/engine/internal/task"
)

type blockingCloseOutput struct {
	*failedOutputAuthority
	entered, release chan struct{}
}

func (o *blockingCloseOutput) Close() error {
	close(o.entered)
	<-o.release
	return o.failedOutputAuthority.Close()
}
func TestReceiveCompletionWaitsForOutputCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		output := &blockingCloseOutput{failedOutputAuthority: &failedOutputAuthority{bindErr: errors.New("bind failed"), closeErr: errors.New("close failed")}, entered: make(chan struct{}), release: make(chan struct{})}
		done := make(chan task.Completion[Result], 1)
		go func() {
			done <- Run(context.Background(), Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return output, nil })}, Dependencies{})
		}()
		<-output.entered
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("result preceded authority cleanup")
		default:
		}
		close(output.release)
		result := <-done
		if output.closed != 1 || !errors.Is(result.CleanupError, output.closeErr) {
			t.Fatalf("cleanup=%+v closed=%d", result, output.closed)
		}
	})
}

func TestConcurrentGenerationCloseJoinsAdmissionOnce(t *testing.T) {
	relay := newFakeReceiverContentSuspension()
	release := make(chan struct{})
	relay.resumeGate = release
	claimGate := make(chan struct{})
	plan, err := ConnectivityAuto.receiverPlan()
	if err != nil {
		t.Fatal(err)
	}
	admission, err := newReceiverContentAdmission(plan, relay, receiverAdmissionHooks{claimGate: claimGate})
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.AdmitRelay(); err != nil {
		t.Fatal(err)
	}
	close(claimGate)
	<-relay.resumeEvent

	execution := &getTransferExecution{admission: admission}
	returned := make(chan struct{}, 2)
	go func() { execution.CloseWithReason(ReceiverLocalStopRuntimeSessionFailure); returned <- struct{}{} }()
	go func() { execution.Close(); returned <- struct{}{} }()
	// sync.Once waits on a mutex, which synctest cannot treat as durably blocked.
	// This bounded check proves neither competing closer returns before Resume finishes.
	const joinCheckWindow = 10 * time.Millisecond
	select {
	case <-returned:
		t.Fatal("concurrent cleanup skipped admission join")
	case <-time.After(joinCheckWindow):
	}
	close(release)
	<-returned
	<-returned
	if relay.count() != 1 {
		t.Fatalf("resume count=%d", relay.count())
	}
}

func TestDetachedReceiveObserverPreservesExecutionAndDurableSourceLoss(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	stream, _ := diagnostics.NewStream(1, nil)
	current, err := task.Start(context.Background(), task.Config[Result]{
		Diagnostics: stream.Sink(), ID: "receive-detached", Now: time.Now, Random: rand.Reader, CleanupTimeout: time.Second, ObservationCapacity: 1,
		Run: func(ctx context.Context, control task.Control) task.Completion[Result] {
			observation := newObservation(control, true, nil)
			close(entered)
			<-release
			for range int(protocolObservationCapacity) + 1 {
				observation.protocol.TryPublish(sessionobs.ProtocolOperationObservation{})
			}
			observation.protocol.RecordDropped(5)
			observation.complete()
			return task.Completion[Result]{Outcome: task.OutcomeSuccess, Err: ctx.Err()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := current.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("detached wait=%v", err)
	}
	close(release)
	result, err := current.Wait(context.Background())
	if err != nil || result.Err != nil || result.Outcome != task.OutcomeSuccess || result.Observations.CapacityDropped == 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if cut := stream.Close(); cut.CapacityDropped < 5 {
		t.Fatalf("trace loss=%+v", cut)
	}

}
