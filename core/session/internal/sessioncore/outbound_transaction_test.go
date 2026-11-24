package sessioncore

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
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
		wantEvidence    protocolsession.SendEvidence
		wantStatus      sessionobs.SendStatus
		wantNotSent     bool
		wantError       bool
	}{
		{name: "unknown then delivered", firstUnknown: true, secondDelivered: true, wantEvidence: protocolsession.SendDelivered, wantStatus: sessionobs.SendStatusConfirmed},
		{name: "unknown then dropped", firstUnknown: true, wantEvidence: protocolsession.SendUncertain, wantStatus: sessionobs.SendStatusUncertain, wantError: true},
		{name: "all pretransport dropped", wantEvidence: protocolsession.SendNotSent, wantStatus: sessionobs.SendStatusNotSent, wantNotSent: true, wantError: true},
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

			operationID := protocolsession.OperationID(110)
			request, _ := protocolsession.NewMessage(
				protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
			)
			op := admitTestSenderOperation(t, runtime, firstIdentity, request)
			body, _ := contentflow.EncodeOperationComplete(1)
			outbound := &Responses{runtime: runtime}
			evidence, sendErr := outbound.SendControl(
				context.Background(), op, protocolsession.MessageOperationComplete, body,
			)
			stopWriters()
			if firstDone != nil {
				<-firstDone
			}
			if secondDone != nil {
				<-secondDone
			}
			if evidence != test.wantEvidence || (sendErr != nil) != test.wantError || protocolsession.IsNotSent(sendErr) != test.wantNotSent {
				t.Fatalf("evidence=%d want=%d sendErr=%v wantError=%v wantNotSent=%v",
					evidence, test.wantEvidence, sendErr, test.wantError, test.wantNotSent)
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
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 4},
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

	operationID := protocolsession.OperationID(115)
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
	evidence, sendErr := outbound.SendControl(context.Background(), op, protocolsession.MessageOperationComplete, body)
	stopWriters()
	wg.Wait()
	// Initial lane's closed memoryChannel returns an unclassified io.ErrClosedPipe
	// (uncertain), while remaining lanes reject pre-transport; the mixed outcome
	// must stay uncertain rather than letting later pre-transport errors win.
	if sendErr == nil || evidence != protocolsession.SendUncertain || protocolsession.IsNotSent(sendErr) {
		t.Fatalf("exhausted response = (evidence=%d, notSent=%v, err=%v), want (SendUncertain, false, non-nil)",
			evidence, protocolsession.IsNotSent(sendErr), sendErr)
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
	operationID := protocolsession.OperationID(111)
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

	operationID := protocolsession.OperationID(112)
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
	secondOperation := protocolsession.OperationID(114)
	secondRequest, _ := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks, &secondOperation, []byte{0xa1, 0x00, 0x02},
	)
	secondOp := admitTestSenderOperation(t, runtime, fallbackIdentity, secondRequest)
	secondFragments, _ := contentflow.FragmentRecord(secondOperation, []byte("second-record"))
	if err := outbound.SendFragment(context.Background(), secondOp, secondFragments[0]); err != nil {
		t.Fatalf("second operation fragment: %v", err)
	}
	completeBody, _ := contentflow.EncodeOperationComplete(1)
	if evidence, err := outbound.SendControl(
		context.Background(), secondOp, protocolsession.MessageOperationComplete, completeBody,
	); err != nil || evidence != protocolsession.SendDelivered {
		t.Fatalf("second operation = (evidence=%d, err=%v)", evidence, err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 2 {
		t.Fatalf("post-cancel continuation active=%d tombstones=%d", runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	close(blockedChannel.release)
	stopWriters()
	<-blockedDone
	<-fallbackDone
}

type twoLaneFailureSetup struct {
	runtime    *Runtime
	recorder   *protocolTraceRecorder
	firstID    session.LaneIdentity
	firstCh    *failingRuntimeLaneChannel
	secondCh   *failingRuntimeLaneChannel
	stopAndEnd func()
}

func setupTwoFailingLanes(t *testing.T, firstErr, secondErr error) twoLaneFailureSetup {
	t.Helper()
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)

	firstBase, firstPeer := newMemoryChannelPair()
	secondBase, secondPeer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = firstPeer.Close()
		_ = secondPeer.Close()
	})
	firstCh := &failingRuntimeLaneChannel{memoryChannel: firstBase, err: firstErr}
	secondCh := &failingRuntimeLaneChannel{memoryChannel: secondBase, err: secondErr}
	firstID := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	secondID := session.LaneIdentity{ID: runtime.initial.ID + 2, Epoch: 1}
	firstLane, err := runtime.lanes.add(firstID, firstCh, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	secondLane, err := runtime.lanes.add(secondID, secondCh, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	writerCtx, stopWriters := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = firstLane.writer.Run(writerCtx) })
	wg.Go(func() { _ = secondLane.writer.Run(writerCtx) })
	stopped := false
	stopAndEnd := func() {
		if stopped {
			return
		}
		stopped = true
		stopWriters()
		wg.Wait()
	}
	t.Cleanup(stopAndEnd)
	return twoLaneFailureSetup{
		runtime: runtime, recorder: recorder, firstID: firstID,
		firstCh: firstCh, secondCh: secondCh, stopAndEnd: stopAndEnd,
	}
}

func TestCrossLaneMixedSendEvidencePreservesUncertaintyInBothOrders(t *testing.T) {
	uncertainCause := errors.New("transport acceptance unknown")
	pretransportCause := errors.New("transport rejected before admission")

	for _, test := range []struct {
		name         string
		firstErr     error
		secondErr    error
		wantEvidence protocolsession.SendEvidence
		wantStatus   sessionobs.SendStatus
		wantNotSent  bool
	}{
		{
			name:         "uncertain then not-sent",
			firstErr:     uncertainCause,
			secondErr:    framechannel.RejectSend(pretransportCause),
			wantEvidence: protocolsession.SendUncertain,
			wantStatus:   sessionobs.SendStatusUncertain,
			wantNotSent:  false,
		},
		{
			name:         "not-sent then uncertain",
			firstErr:     framechannel.RejectSend(pretransportCause),
			secondErr:    uncertainCause,
			wantEvidence: protocolsession.SendUncertain,
			wantStatus:   sessionobs.SendStatusUncertain,
			wantNotSent:  false,
		},
		{
			name:         "not-sent then not-sent",
			firstErr:     framechannel.RejectSend(pretransportCause),
			secondErr:    framechannel.RetireSend(errors.New("lane retired before admission")),
			wantEvidence: protocolsession.SendNotSent,
			wantStatus:   sessionobs.SendStatusNotSent,
			wantNotSent:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			setup := setupTwoFailingLanes(t, test.firstErr, test.secondErr)
			operationID := protocolsession.OperationID(140)
			request, _ := protocolsession.NewMessage(
				protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
			)
			op := admitTestSenderOperation(t, setup.runtime, setup.firstID, request)
			body, _ := contentflow.EncodeOperationComplete(1)
			outbound := &Responses{runtime: setup.runtime}

			evidence, sendErr := outbound.SendControl(
				context.Background(), op, protocolsession.MessageOperationComplete, body,
			)
			setup.stopAndEnd()

			if setup.firstCh.sends.Load() != 1 || setup.secondCh.sends.Load() != 1 {
				t.Fatalf("lane attempts = (%d, %d), want (1, 1)", setup.firstCh.sends.Load(), setup.secondCh.sends.Load())
			}
			if evidence != test.wantEvidence || sendErr == nil {
				t.Fatalf("SendControl = (evidence=%d, err=%v), want evidence=%d non-nil error", evidence, sendErr, test.wantEvidence)
			}
			if protocolsession.IsNotSent(sendErr) != test.wantNotSent {
				t.Fatalf("IsNotSent(%v) = %v, want %v", sendErr, protocolsession.IsNotSent(sendErr), test.wantNotSent)
			}
			if errors.Is(sendErr, protocolsession.ErrNotSent) != test.wantNotSent {
				t.Fatalf("errors.Is(%v, ErrNotSent) = %v, want %v", sendErr, errors.Is(sendErr, protocolsession.ErrNotSent), test.wantNotSent)
			}
			if !errors.Is(sendErr, pretransportCause) {
				t.Fatalf("lost pre-transport root cause: %v", sendErr)
			}
			if !test.wantNotSent && !errors.Is(sendErr, uncertainCause) {
				t.Fatalf("lost uncertain root cause: %v", sendErr)
			}
			facts := setup.recorder.facts()
			if len(facts) != 1 {
				t.Fatalf("facts = %+v", facts)
			}
			observed, ok := facts[0].(sessionobs.ResponseSendObserved)
			if !ok || observed.Status() != test.wantStatus || observed.Attempts() != 2 {
				t.Fatalf("observed = %+v, want status=%v attempts=2", facts[0], test.wantStatus)
			}
		})
	}
}

type graceTestClock struct{ now time.Time }

func (c *graceTestClock) Now() time.Time { return c.now }

type graceTestCatalog struct {
	id     catalog.NodeID
	record catalog.NodeRecord
}

func (c graceTestCatalog) Node(_ context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	if c.id == id {
		return c.record, true, nil
	}
	return catalog.NodeRecord{}, false, nil
}

type graceTestSource struct {
	data     []byte
	modified catalog.ModifiedTime
	opens    atomic.Int32
	closes   atomic.Int32
}

func (s *graceTestSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	s.opens.Add(1)
	return &graceTestHandle{source: s}, nil
}

type graceTestHandle struct {
	source *graceTestSource
	closed atomic.Int32
}

func (h *graceTestHandle) ExactSize() uint64                  { return uint64(len(h.source.data)) }
func (h *graceTestHandle) ModifiedTime() catalog.ModifiedTime { return h.source.modified }
func (h *graceTestHandle) Verify(context.Context) error       { return nil }
func (h *graceTestHandle) ReadAt(_ context.Context, dst []byte, offset uint64) (int, error) {
	if offset >= uint64(len(h.source.data)) {
		return 0, io.EOF
	}
	n := copy(dst, h.source.data[offset:])
	if n != len(dst) {
		return n, io.EOF
	}
	return n, nil
}
func (h *graceTestHandle) Close() error {
	if h.closed.CompareAndSwap(0, 1) {
		h.source.closes.Add(1)
	}
	return nil
}

func TestOpenRevisionsMixedCrossLaneFailurePreservesRecoveryGraceInBothOrders(t *testing.T) {
	uncertainErr := errors.New("lane transport ownership unknown")
	pretransportErr := framechannel.RejectSend(errors.New("lane rejected pre-transport"))

	for _, test := range []struct {
		name               string
		firstErr           error
		secondErr          error
		wantStage          contentflow.SenderDecisionStage
		wantGraceRetained  bool
		wantClosesAfterEnd int32
		wantOpensOnReopen  int32
	}{
		{
			name:               "uncertain then not-sent keeps recovery grace",
			firstErr:           uncertainErr,
			secondErr:          pretransportErr,
			wantStage:          contentflow.SenderDecisionLeaseDetached,
			wantGraceRetained:  true,
			wantClosesAfterEnd: 0,
			wantOpensOnReopen:  1,
		},
		{
			name:               "not-sent then uncertain keeps recovery grace",
			firstErr:           pretransportErr,
			secondErr:          uncertainErr,
			wantStage:          contentflow.SenderDecisionLeaseDetached,
			wantGraceRetained:  true,
			wantClosesAfterEnd: 0,
			wantOpensOnReopen:  1,
		},
		{
			name:               "all not-sent releases handle without grace",
			firstErr:           pretransportErr,
			secondErr:          framechannel.RetireSend(errors.New("second lane retired")),
			wantStage:          contentflow.SenderDecisionLeaseUndelivered,
			wantGraceRetained:  false,
			wantClosesAfterEnd: 1,
			wantOpensOnReopen:  2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			share := catalog.ShareInstance{1, 2, 3, 4}
			fileID := catalog.FileID{5, 6, 7, 8}
			parentID := catalog.DirectoryID{9, 10, 11, 12}
			chunkSize := uint32(catalog.MinChunkSize)
			data := make([]byte, chunkSize)
			locator, err := catalog.NewSourceReference([]byte("object:grace.bin"))
			if err != nil {
				t.Fatal(err)
			}
			sourceID, err := catalog.NewSourceIdentity([]byte("grace-src"))
			if err != nil {
				t.Fatal(err)
			}
			versionCand, err := catalog.NewVersionCandidate([]byte("grace-ver"))
			if err != nil {
				t.Fatal(err)
			}
			modified, err := catalog.NewModifiedTime(1_000, 0, catalog.TimePrecisionSeconds)
			if err != nil {
				t.Fatal(err)
			}
			node, err := catalog.NewFileNodeRecord(
				fileID, parentID, "grace.bin", locator, sourceID, versionCand, uint64(len(data)), modified,
			)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := content.NewHandlePool(content.DefaultHandlePoolConfig())
			if err != nil {
				t.Fatal(err)
			}
			processCache, err := contentflow.NewProcessCacheBudget(uint64(chunkSize) * 4)
			if err != nil {
				t.Fatal(err)
			}
			cache, err := contentflow.NewSharedBlockCache(share, uint64(chunkSize)*4, processCache)
			if err != nil {
				t.Fatal(err)
			}
			source := &graceTestSource{data: data, modified: modified}
			store, err := content.NewRevisionStore(content.RevisionStoreConfig{
				ShareInstance: share,
				ChunkSize:     chunkSize,
				Catalog:       graceTestCatalog{id: fileID.NodeID(), record: node},
				Source:        source,
				HandlePool:    pool,
				ShareLimits:   content.DefaultShareCapacityLimits(),
				Clock:         &graceTestClock{now: time.Unix(10_000, 0)},
				Cache:         cache,
			})
			if err != nil {
				_ = pool.Close()
				t.Fatal(err)
			}
			firstSession, err := store.RegisterSession(content.SessionConfig{
				SessionID: "mixed-send-session-1", Limits: content.DefaultSessionCapacityLimits(),
			})
			if err != nil {
				t.Fatal(err)
			}
			secondSession, err := store.RegisterSession(content.SessionConfig{
				SessionID: "mixed-send-session-2", Limits: content.DefaultSessionCapacityLimits(),
			})
			if err != nil {
				t.Fatal(err)
			}
			firstService, err := contentflow.NewSenderService(contentflow.SenderServiceConfig{
				Store: firstSession, Encoder: records.Encoder{}, Cache: cache,
			})
			if err != nil {
				t.Fatal(err)
			}
			secondService, err := contentflow.NewSenderService(contentflow.SenderServiceConfig{
				Store: secondSession, Encoder: records.Encoder{}, Cache: cache,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = secondService.Close()
				_ = firstService.Close()
				cache.Close()
				_ = store.Close()
				_ = pool.Close()
			})

			setup := setupTwoFailingLanes(t, test.firstErr, test.secondErr)
			decisions := make(chan contentflow.SenderDecisionTrace, 4)
			handler, err := contentflow.NewSenderHandler(contentflow.SenderHandlerConfig{
				Service:  firstService,
				Outbound: NewResponses(setup.runtime, ResponseConfig{}),
				DecisionTracer: contentflow.SenderDecisionTraceFunc(func(trace contentflow.SenderDecisionTrace) {
					decisions <- trace
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			handlerCtx, stopHandler := context.WithCancel(context.Background())
			handlerDone := make(chan error, 1)
			go func() { handlerDone <- handler.Run(handlerCtx) }()

			openReq, err := contentflow.NewOpenRequest([]contentflow.OpenItem{{FileID: fileID}})
			if err != nil {
				t.Fatal(err)
			}
			openBody, err := contentflow.EncodeOpenRequest(openReq)
			if err != nil {
				t.Fatal(err)
			}
			operationID := protocolsession.OperationID(141)
			request, err := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, openBody)
			if err != nil {
				t.Fatal(err)
			}
			op := admitTestSenderOperation(t, setup.runtime, setup.firstID, request)
			if err := handler.HandleMessage(context.Background(), op, request); err != nil {
				t.Fatal(err)
			}

			var decision contentflow.SenderDecisionTrace
			select {
			case decision = <-decisions:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for open settlement decision")
			}
			stopHandler()
			<-handlerDone
			setup.stopAndEnd()

			if setup.firstCh.sends.Load() != 1 || setup.secondCh.sends.Load() != 1 {
				t.Fatalf("lane attempts = (%d, %d), want (1, 1)", setup.firstCh.sends.Load(), setup.secondCh.sends.Load())
			}
			if decision.Stage != test.wantStage {
				t.Fatalf("decision stage = %d, want %d", decision.Stage, test.wantStage)
			}
			if got := source.closes.Load(); got != test.wantClosesAfterEnd {
				t.Fatalf("handle closes after failed open = %d, want %d", got, test.wantClosesAfterEnd)
			}
			wantReclaimable := uint64(0)
			if test.wantGraceRetained {
				wantReclaimable = 1
			}
			if got := store.CapacitySnapshot().Share().ReclaimableStableHandles(); got != wantReclaimable {
				t.Fatalf("reclaimable grace handles = %d, want %d", got, wantReclaimable)
			}
			if _, err := secondService.Open(context.Background(), openReq); err != nil {
				t.Fatalf("follow-up open failed: %v", err)
			}
			if got := source.opens.Load(); got != test.wantOpensOnReopen {
				t.Fatalf("source opens after follow-up open = %d, want %d", got, test.wantOpensOnReopen)
			}
		})
	}
}
