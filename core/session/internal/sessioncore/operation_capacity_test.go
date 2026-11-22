package sessioncore

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRPCWaitsForActiveCapacityAndProceedsWithoutTombstoneBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime, _ := newUnstartedRuntimeWithPolicy(t, protocolsession.RoleReceiver,
			protocolsession.OperationLimits{MaxActive: 1, MaxRecentTombstones: 1}, nil)
		recorder := newProtocolTraceRecorder(runtime)
		physical := runtime.lanes.active[runtime.initial.ID]
		writerContext, stopWriter := context.WithCancel(context.Background())
		writerDone := make(chan error, 1)
		go func() { writerDone <- physical.writer.Run(writerContext) }()
		defer func() { stopWriter(); <-writerDone }()
		client := NewRequests(runtime, nil)
		body := []byte{0xa1, 0x00, 0x01}
		first, err := client.Begin(context.Background(), protocolsession.MessageListChildren, body)
		if err != nil {
			t.Fatal(err)
		}
		type begun struct {
			call *Call
			err  error
		}
		begin := func(ctx context.Context) <-chan begun {
			done := make(chan begun, 1)
			go func() {
				call, err := client.Begin(ctx, protocolsession.MessageListChildren, body)
				done <- begun{call, err}
			}()
			return done
		}
		second := begin(context.Background())
		synctest.Wait()
		select {
		case result := <-second:
			t.Fatalf("active capacity did not wait: %v", result.err)
		default:
		}
		if err := client.CancelAndEnd(first, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		result := <-second
		if result.err != nil {
			t.Fatal(result.err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		third := begin(ctx)
		synctest.Wait()
		cancel()
		if cancelled := <-third; !errors.Is(cancelled.err, context.Canceled) {
			t.Fatalf("cancelled capacity wait: %v", cancelled.err)
		}
		if err := client.CancelAndEnd(result.call, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		// Exceeding MaxRecentTombstones must NOT block or sleep; oldest unpinned tombstone evicts FIFO.
		fourth, err := client.Begin(context.Background(), protocolsession.MessageListChildren, body)
		if err != nil {
			t.Fatalf("fourth begin after tombstone cap: %v", err)
		}
		if err := client.CancelAndEnd(fourth, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if runtime.ctx.Err() != nil || !physical.writer.Accepting() || runtime.operations.ActiveCount() != 0 {
			t.Fatal("capacity pressure ended the session or leaked operation authority")
		}
		if runtime.operations.TombstoneCount() > 1 {
			t.Fatalf("tombstone count %d exceeded MaxRecentTombstones=1", runtime.operations.TombstoneCount())
		}
		traces := recorder.snapshot()
		for _, stage := range []sessionobs.ProtocolOperationStage{
			sessionobs.ProtocolOperationReceiverWaitingActiveCapacity,
			sessionobs.ProtocolOperationReceiverAdmissionReady,
		} {
			found := false
			for _, event := range traces {
				if event.Stage == stage && !event.OperationID.IsZero() && event.ProtocolSessionID == runtime.sessionID {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing correlated admission trace stage %d", stage)
			}
		}
	})
}
