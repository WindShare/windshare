package sessioncore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type failingRuntimeLaneChannel struct {
	*memoryChannel
	err   error
	sends atomic.Int32
}

func (channel *failingRuntimeLaneChannel) Send(context.Context, framechannel.Frame) error {
	channel.sends.Add(1)
	return channel.err
}

func (channel *failingRuntimeLaneChannel) SendTerminal(context.Context, framechannel.Frame) error {
	channel.sends.Add(1)
	return channel.err
}

type blockingRuntimeLaneChannel struct {
	*memoryChannel
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (channel *blockingRuntimeLaneChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.once.Do(func() { close(channel.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-channel.release:
		return channel.memoryChannel.Send(ctx, frame)
	}
}

func (channel *blockingRuntimeLaneChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

type countingRuntimeLaneChannel struct {
	*memoryChannel
	sends atomic.Int32
}

func (channel *countingRuntimeLaneChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	return channel.memoryChannel.Send(ctx, frame)
}

func (channel *countingRuntimeLaneChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

func admitTestSenderOperation(
	t *testing.T,
	runtime *Runtime,
	identity session.LaneIdentity,
	request protocolsession.Message,
) protocolsession.Operation {
	t.Helper()
	admission, err := runtime.operations.ObserveInbound(
		protocolsession.DirectionReceiverToSender, identity, request,
	)
	if err != nil || admission.disposition != protocolsession.OperationDeliver || admission.op.IsZero() {
		t.Fatalf("admit sender operation: disposition=%d err=%v", admission.disposition, err)
	}
	return admission.op
}

func TestSenderControlTransactionAggregatesAttemptsAndDrainsAuthority(t *testing.T) {
	for _, test := range []struct {
		name            string
		firstUnknown    bool
		secondDelivered bool
		wantStatus      sessionobs.SendStatus
		wantNotSent     bool
		wantError       bool
	}{
		{name: "unknown then delivered", firstUnknown: true, secondDelivered: true, wantStatus: sessionobs.SendStatusConfirmed},
		{name: "unknown then dropped", firstUnknown: true, wantStatus: sessionobs.SendStatusUncertain, wantError: true},
		{name: "all pretransport dropped", wantStatus: sessionobs.SendStatusNotSent, wantNotSent: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			recorder := newProtocolTraceRecorder(runtime)
			initial, err := runtime.lanes.selectLane(&runtime.initial)
			if err != nil {
				t.Fatal(err)
			}
			firstBase, firstPeer := newMemoryChannelPair()
			t.Cleanup(func() { _ = firstPeer.Close() })
			firstIdentity := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
			first, err := runtime.lanes.add(
				firstIdentity,
				&failingRuntimeLaneChannel{memoryChannel: firstBase, err: errors.New("physical send is uncertain")},
				permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			writerContext, stopWriters := context.WithCancel(context.Background())
			var firstDone, secondDone chan error
			if test.firstUnknown {
				firstDone = make(chan error, 1)
				go func() { firstDone <- first.writer.Run(writerContext) }()
			} else {
				stopped, stop := context.WithCancel(context.Background())
				stop()
				_ = first.writer.Run(stopped)
			}
			if test.secondDelivered {
				secondDone = make(chan error, 1)
				go func() { secondDone <- initial.writer.Run(writerContext) }()
			} else {
				stopped, stop := context.WithCancel(context.Background())
				stop()
				_ = initial.writer.Run(stopped)
			}

			operationID := id16[protocolsession.OperationID](110)
			request, _ := protocolsession.NewMessage(
				protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
			)
			op := admitTestSenderOperation(t, runtime, firstIdentity, request)
			body, _ := contentflow.EncodeOperationComplete(1)
			outbound := &Responses{runtime: runtime}
			sendErr := outbound.SendControl(
				context.Background(), op, protocolsession.MessageOperationComplete, body,
			)
			stopWriters()
			if firstDone != nil {
				<-firstDone
			}
			if secondDone != nil {
				<-secondDone
			}
			if (sendErr != nil) != test.wantError || protocolsession.IsNotSent(sendErr) != test.wantNotSent {
				t.Fatalf("sendErr=%v wantError=%v wantNotSent=%v", sendErr, test.wantError, test.wantNotSent)
			}
			facts := recorder.facts()
			if len(facts) != 1 {
				t.Fatalf("facts=%+v", facts)
			}
			observed, ok := facts[0].(sessionobs.ResponseSendObserved)
			if !ok || observed.Status() != test.wantStatus {
				t.Fatalf("observed=%+v wantStatus=%v", facts[0], test.wantStatus)
			}
			if test.firstUnknown {
				wantAttempts := uint32(1)
				wantLane := firstIdentity
				if test.secondDelivered {
					wantAttempts = 2
					wantLane = runtime.initial
				}
				if observed.Attempts() != wantAttempts || observed.Lane() != wantLane {
					t.Fatalf("observed attempts=%d lane=%+v want attempts=%d lane=%+v",
						observed.Attempts(), observed.Lane(), wantAttempts, wantLane)
				}
			}
			if errors.Is(sendErr, session.ErrRuntimeClosed) {
				t.Fatalf("live-session attempt exhaustion was misclassified as runtime closure: %v", sendErr)
			}
			if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
				t.Fatalf("active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
			}
		})
	}
}

func TestOutboundResponseAttemptsEveryUsableLaneOnceOnExhaustion(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(time.Unix(1, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	runtime, initialChannel := newUnstartedRuntimeWithPolicy(
		t,
		protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4},
		now,
	)
	if err := initialChannel.Close(); err != nil {
		t.Fatal(err)
	}
	physicalFailure := framechannel.RejectSend(errors.New("physical lane rejected frame"))
	channels := make([]*failingRuntimeLaneChannel, 0, protocolsession.DefaultMaxLogicalLanes-1)
	writerContext, stopWriters := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	initialLane, _ := runtime.lanes.selectLane(&runtime.initial)
	wg.Go(func() { _ = initialLane.writer.Run(writerContext) })
	for laneIndex := 1; laneIndex < protocolsession.DefaultMaxLogicalLanes; laneIndex++ {
		base, peer := newMemoryChannelPair()
		t.Cleanup(func() { _ = peer.Close() })
		ch := &failingRuntimeLaneChannel{memoryChannel: base, err: physicalFailure}
		channels = append(channels, ch)
		identity := session.LaneIdentity{
			ID:    uint32(laneIndex + 1),
			Epoch: uint32(protocolsession.DefaultMaxLogicalLanes + laneIndex),
		}
		lane, err := runtime.lanes.add(identity, ch, permissiveInboundAuthenticator(), false)
		if err != nil {
			t.Fatalf("add lane %+v: %v", identity, err)
		}
		wg.Go(func() { _ = lane.writer.Run(writerContext) })
	}

	operationID := id16[protocolsession.OperationID](115)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks,
		&operationID,
		[]byte{0xa1, 0x00, 0x01},
	)
	if err != nil {
		t.Fatal(err)
	}
	op := admitTestSenderOperation(t, runtime, runtime.initial, request)
	body, _ := contentflow.EncodeOperationComplete(0)
	outbound := &Responses{runtime: runtime}
	sendErr := outbound.SendControl(context.Background(), op, protocolsession.MessageOperationComplete, body)
	stopWriters()
	wg.Wait()
	if sendErr == nil {
		t.Fatal("exhausted response unexpectedly succeeded")
	}
	for index, ch := range channels {
		if ch.sends.Load() != 1 {
			t.Fatalf("lane %d attempted %d times, want 1", index+2, ch.sends.Load())
		}
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatalf("bounded lane exhaustion terminated the runtime: %v", runtime.Err())
	default:
	}
}

func TestFragmentFailureRetainsRouteUntilOperationErrorDrainsAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	initial, _ := runtime.lanes.selectLane(&runtime.initial)
	firstBase, firstPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = firstPeer.Close() })
	firstIdentity := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	first, err := runtime.lanes.add(
		firstIdentity,
		&failingRuntimeLaneChannel{memoryChannel: firstBase, err: errors.New("fragment acceptance unknown")},
		permissiveInboundAuthenticator(), false,
	)
	if err != nil {
		t.Fatal(err)
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	_ = initial.writer.Run(stopped)
	writerDone := make(chan error, 1)
	go func() { writerDone <- first.writer.Run(context.Background()) }()
	operationID := id16[protocolsession.OperationID](111)
	request, _ := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
	)
	op := admitTestSenderOperation(t, runtime, firstIdentity, request)
	fragments, _ := contentflow.FragmentRecord(operationID, []byte("record"))
	outbound := &Responses{runtime: runtime}
	if err := outbound.SendFragment(context.Background(), op, fragments[0]); err == nil {
		t.Fatal("all-lane fragment failure unexpectedly succeeded")
	}
	<-writerDone
	if !op.IsActive() || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("fragment failure prematurely drained op: active=%d", runtime.operations.ActiveCount())
	}
	if err := outbound.SendOperationError(context.Background(), op, contentflow.OperationFailure{
		Scope: contentflow.BlockErrorScope, Code: contentflow.BlockCodeTimeout, Message: "Block failed",
	}); err == nil {
		t.Fatal("all-lane operation error unexpectedly succeeded")
	}
	if op.IsActive() || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestUnsettledCallerCancelDoesNotMigrateOrTerminateSession(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	blockedBase, blockedPeer := newMemoryChannelPair()
	fallbackBase, fallbackPeer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = blockedPeer.Close()
		_ = fallbackPeer.Close()
	})
	blockedChannel := &blockingRuntimeLaneChannel{
		memoryChannel: blockedBase, started: make(chan struct{}), release: make(chan struct{}),
	}
	blockedIdentity := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	blocked, err := runtime.lanes.add(blockedIdentity, blockedChannel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	fallbackChannel := &countingRuntimeLaneChannel{memoryChannel: fallbackBase}
	fallbackIdentity := session.LaneIdentity{ID: runtime.initial.ID + 2, Epoch: 1}
	fallback, err := runtime.lanes.add(fallbackIdentity, fallbackChannel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	writerContext, stopWriters := context.WithCancel(context.Background())
	blockedDone := make(chan error, 1)
	fallbackDone := make(chan error, 1)
	go func() { blockedDone <- blocked.writer.Run(writerContext) }()
	go func() { fallbackDone <- fallback.writer.Run(writerContext) }()

	operationID := id16[protocolsession.OperationID](112)
	request, _ := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
	)
	op := admitTestSenderOperation(t, runtime, blockedIdentity, request)
	fragments, _ := contentflow.FragmentRecord(operationID, []byte("record"))
	outbound := &Responses{runtime: runtime}
	sendContext, cancelSend := context.WithCancel(context.Background())
	sendDone := make(chan error, 1)
	go func() { sendDone <- outbound.SendFragment(sendContext, op, fragments[0]) }()
	<-blockedChannel.started
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonLaneRace)
	cancelMessage, _ := protocolsession.NewMessage(protocolsession.MessageCancel, &operationID, cancelBody)
	if disposition, err := runtime.inbound.RouteInbound(fallbackIdentity, cancelMessage); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("inbound cancel disposition=%d error=%v", disposition, err)
	}
	cancelSend()
	if err := <-sendDone; !errors.Is(err, context.Canceled) || errors.Is(err, session.ErrRuntimeClosed) {
		t.Fatalf("cancelled transaction error=%v", err)
	}
	if fallbackChannel.sends.Load() != 0 {
		t.Fatalf("unsettled transaction migrated %d time(s)", fallbackChannel.sends.Load())
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatal("ordinary caller cancellation terminated the protocol session")
	default:
	}
	secondOperation := id16[protocolsession.OperationID](114)
	secondRequest, _ := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks, &secondOperation, []byte{0xa1, 0x00, 0x02},
	)
	secondOp := admitTestSenderOperation(t, runtime, fallbackIdentity, secondRequest)
	secondFragments, _ := contentflow.FragmentRecord(secondOperation, []byte("second-record"))
	if err := outbound.SendFragment(context.Background(), secondOp, secondFragments[0]); err != nil {
		t.Fatalf("second operation fragment: %v", err)
	}
	completeBody, _ := contentflow.EncodeOperationComplete(1)
	if err := outbound.SendControl(
		context.Background(), secondOp, protocolsession.MessageOperationComplete, completeBody,
	); err != nil {
		t.Fatalf("second operation error=%v", err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 2 {
		t.Fatalf("post-cancel continuation active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	close(blockedChannel.release)
	stopWriters()
	<-blockedDone
	<-fallbackDone
}
