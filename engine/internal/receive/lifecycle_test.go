package receive

import (
	"context"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/sessionruntime"
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

type blockingJoinAdmission struct {
	receiverContentAdmission
	closes           atomic.Int32
	entered, release chan struct{}
}

func (a *blockingJoinAdmission) Close() {
	if a.closes.Add(1) == 1 {
		close(a.entered)
	}
}
func (a *blockingJoinAdmission) Wait() { <-a.release }
func TestConcurrentGenerationCloseJoinsAdmissionAndMonitorOnce(t *testing.T) {
	admission := &blockingJoinAdmission{entered: make(chan struct{}), release: make(chan struct{})}
	monitor := make(chan struct{})
	execution := &getTransferExecution{admission: admission, monitorDone: monitor}
	returned := make(chan struct{}, 2)
	go func() { execution.CloseWithReason(ReceiverLocalStopRuntimeSessionFailure); returned <- struct{}{} }()
	<-admission.entered
	go func() { execution.Close(); returned <- struct{}{} }()
	// sync.Once waits on a mutex, which synctest cannot treat as durably blocked.
	// These short bounded waits prove that competing closers cannot skip either join.
	const joinCheckWindow = 10 * time.Millisecond
	select {
	case <-returned:
		t.Fatal("concurrent cleanup skipped admission join")
	case <-time.After(joinCheckWindow):
	}
	close(admission.release)
	select {
	case <-returned:
		t.Fatal("cleanup skipped monitor join")
	case <-time.After(joinCheckWindow):
	}
	close(monitor)
	<-returned
	<-returned
	if admission.closes.Load() != 1 {
		t.Fatalf("closes=%d", admission.closes.Load())
	}
}

func TestDetachedReceiveObserverPreservesExecutionAndDurableSourceLoss(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	current, err := task.Start(context.Background(), task.Config[Result]{
		ID: "receive-detached", Now: time.Now, Random: rand.Reader, CleanupTimeout: time.Second, ObservationCapacity: 1,
		Run: func(ctx context.Context, control task.Control) task.Completion[Result] {
			observation := newObservation(control, true, nil)
			close(entered)
			<-release
			for range int(protocolObservationCapacity) + 1 {
				observation.protocol.TryPublish(sessionruntime.ProtocolOperationObservation{})
			}
			observation.protocol.RecordDropped(5)
			observation.complete()
			return task.Completion[Result]{Value: Result{ObservationLosses: observation.lossSnapshot()}, Settlement: task.Settlement{Outcome: task.OutcomeSuccess, Err: ctx.Err()}}
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
	if len(result.Value.ObservationLosses) != 1 || result.Value.ObservationLosses[0].Source != ObservationProtocol || result.Value.ObservationLosses[0].Count < 5 {
		t.Fatalf("loss=%+v", result.Value.ObservationLosses)
	}
}
