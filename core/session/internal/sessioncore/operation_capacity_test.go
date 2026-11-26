package sessioncore

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session"
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
		client := NewRequests(runtime)
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
		if cancelled := <-third; cancelled.err != context.Canceled {
			t.Fatalf("cancelled capacity wait = %v, want exact context.Canceled", cancelled.err)
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

func TestBeginCallPreAdmissionContextErrorAlignsWithPrepareCall(t *testing.T) {
	t.Run("capacity deadline returns exact DeadlineExceeded while runtime is live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, _ := newUnstartedRuntimeWithPolicy(t, protocolsession.RoleReceiver,
				protocolsession.OperationLimits{MaxActive: 1, MaxRecentTombstones: 4}, nil)
			physical := runtime.lanes.active[runtime.initial.ID]
			writerCtx, stopWriter := context.WithCancel(context.Background())
			writerDone := make(chan error, 1)
			go func() { writerDone <- physical.writer.Run(writerCtx) }()
			defer func() { stopWriter(); <-writerDone }()

			client := NewRequests(runtime)
			body := []byte{0xa1, 0x00, 0x01}
			first, err := client.Begin(context.Background(), protocolsession.MessageListChildren, body)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.CancelAndEnd(first, contentflow.CancelReasonOutputAbort) }()

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err = client.Begin(ctx, protocolsession.MessageListChildren, body)
			if err != context.DeadlineExceeded {
				t.Fatalf("begin deadline wait = %v, want exact context.DeadlineExceeded", err)
			}
		})
	})

	t.Run("queued writer cancel returns exact Canceled while runtime is live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
			base, peer := newMemoryChannelPair()
			defer func() { _ = base.Close(); _ = peer.Close() }()
			blocker := &blockingRuntimeLaneChannel{
				memoryChannel: base, started: make(chan struct{}), release: make(chan struct{}),
			}
			lane2ID := session.LaneIdentity{ID: 2, Epoch: 1}
			lane2, err := runtime.lanes.add(lane2ID, blocker, permissiveInboundAuthenticator(), false)
			if err != nil {
				t.Fatal(err)
			}
			writerCtx, stopWriter := context.WithCancel(context.Background())
			writerDone := make(chan error, 1)
			go func() { writerDone <- lane2.writer.Run(writerCtx) }()
			defer func() { close(blocker.release); stopWriter(); <-writerDone }()

			client := NewRequests(runtime)
			body := []byte{0xa1, 0x00, 0x01}
			// Park lane2 writer on an in-flight frame so the next BeginCall queues before send.
			firstCall, firstMsg, err := client.PrepareCall(context.Background(), protocolsession.MessageListChildren, body)
			if err != nil {
				t.Fatal(err)
			}
			firstDone := make(chan error, 1)
			go func() { firstDone <- client.BeginCall(context.Background(), &lane2ID, firstCall, firstMsg) }()
			<-blocker.started

			ctx, cancel := context.WithCancel(context.Background())
			queuedCall, queuedMsg, err := client.PrepareCall(ctx, protocolsession.MessageListChildren, body)
			if err != nil {
				t.Fatal(err)
			}
			queuedDone := make(chan error, 1)
			go func() { queuedDone <- client.BeginCall(ctx, &lane2ID, queuedCall, queuedMsg) }()
			synctest.Wait()
			cancel()
			if err := <-queuedDone; err != context.Canceled {
				t.Fatalf("queued cancel = %v, want exact context.Canceled", err)
			}
		})
	})

	t.Run("closed runtime keeps ErrRuntimeClosed and NotSent semantics", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		runtime.Start()
		client := NewRequests(runtime)
		call, msg, err := client.PrepareCall(context.Background(), protocolsession.MessageListChildren, []byte{0xa1, 0x00, 0x01})
		if err != nil {
			t.Fatal(err)
		}
		runtime.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = client.BeginCall(ctx, nil, call, msg)
		if !errors.Is(err, session.ErrRuntimeClosed) || !protocolsession.IsNotSent(err) {
			t.Fatalf("closed runtime begin = %v (notSent=%v), want ErrRuntimeClosed + NotSent", err, protocolsession.IsNotSent(err))
		}
	})
}
