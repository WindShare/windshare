package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type failingRuntimeLaneChannel struct {
	*memoryChannel
	err error
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

func (channel *failingRuntimeLaneChannel) Send(context.Context, framechannel.Frame) error {
	return channel.err
}

func (channel *failingRuntimeLaneChannel) SendTerminal(context.Context, framechannel.Frame) error {
	return channel.err
}

func TestSenderControlTransactionAggregatesAttemptsAndDrainsAuthority(t *testing.T) {
	for _, test := range []struct {
		name            string
		firstUnknown    bool
		secondDelivered bool
		wantOutcome     protocolsession.SendOutcome
		wantError       bool
	}{
		{name: "unknown then delivered", firstUnknown: true, secondDelivered: true, wantOutcome: protocolsession.SendOutcomeDelivered},
		{name: "unknown then dropped", firstUnknown: true, wantOutcome: protocolsession.SendOutcomeUnknown, wantError: true},
		{name: "all pretransport dropped", wantOutcome: protocolsession.SendOutcomeDropped, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			initial, err := runtime.lanes.selectLane(&runtime.initial)
			if err != nil {
				t.Fatal(err)
			}
			firstBase, firstPeer := newMemoryChannelPair()
			t.Cleanup(func() { _ = firstPeer.Close() })
			firstIdentity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
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
			operationContext, _ := testOutboundOperationContext(t, runtime, firstIdentity, request)
			body, _ := contentflow.EncodeOperationComplete(1)
			outbound := senderOutbound{
				runtime:    runtime,
				privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
			}
			outcome, sendErr := outbound.SendControl(
				operationContext, protocolsession.MessageOperationComplete, operationID, body,
			)
			stopWriters()
			if firstDone != nil {
				<-firstDone
			}
			if secondDone != nil {
				<-secondDone
			}
			if outcome != test.wantOutcome || (sendErr != nil) != test.wantError {
				t.Fatalf("outcome=%d error=%v", outcome, sendErr)
			}
			if errors.Is(sendErr, ErrRuntimeClosed) {
				t.Fatalf("live-session attempt exhaustion was misclassified as runtime closure: %v", sendErr)
			}
			if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
				t.Fatalf("routes=%d active=%d tombstones=%d", runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
			}
		})
	}
}

func TestOutboundTransactionAttemptsExactlyEveryFullLaneIdentityOnce(t *testing.T) {
	const errorBytesPerLaneBound = 160

	var nanos atomic.Int64
	nanos.Store(time.Unix(1, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	runtime, initialChannel := newUnstartedRuntimeWithPolicy(
		t,
		protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4},
		now,
	)
	if err := initialChannel.Close(); err != nil {
		t.Fatal(err)
	}
	physicalFailure := errors.New("physical lane rejected frame")
	for laneIndex := 1; laneIndex < protocolsession.DefaultMaxLogicalLanes; laneIndex++ {
		base, peer := newMemoryChannelPair()
		t.Cleanup(func() { _ = peer.Close() })
		identity := LaneIdentity{
			ID:    uint32(laneIndex + 1),
			Epoch: uint32(protocolsession.DefaultMaxLogicalLanes + laneIndex),
		}
		if _, err := runtime.lanes.add(
			identity,
			&failingRuntimeLaneChannel{memoryChannel: base, err: physicalFailure},
			permissiveInboundAuthenticator(),
			false,
		); err != nil {
			t.Fatalf("add lane %+v: %v", identity, err)
		}
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
	operationContext, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
	transaction, err := beginOutboundTransaction(runtime, operationContext, operationID)
	if err != nil {
		t.Fatal(err)
	}
	transactionClosed := false
	defer func() {
		if !transactionClosed {
			transaction.Close()
		}
	}()

	attempted := make([]LaneIdentity, 0, protocolsession.DefaultMaxLogicalLanes)
	outcome, runErr := transaction.Run(operationContext, func(
		lane selectedLane,
		_ protocolsession.OutboundReplayPermit,
	) (protocolsession.SendReceipt, error) {
		attempted = append(attempted, lane.identity)
		physicalErr := lane.channel.Send(
			operationContext,
			framechannel.Frame{byte(len(attempted))},
		)
		if physicalErr == nil {
			return protocolsession.SendReceipt{}, errors.New("physical lane unexpectedly accepted frame")
		}
		return protocolsession.SendReceipt{}, errors.Join(protocolsession.ErrWriterStopped, physicalErr)
	})
	if outcome != protocolsession.SendOutcomeDropped || runErr == nil ||
		!errors.Is(runErr, protocolsession.ErrWriterStopped) || !errors.Is(runErr, ErrLaneUnavailable) {
		t.Fatalf("exhausted transaction outcome=%d error=%v", outcome, runErr)
	}
	if len(attempted) != protocolsession.DefaultMaxLogicalLanes {
		t.Fatalf("physical attempts=%d, want %d", len(attempted), protocolsession.DefaultMaxLogicalLanes)
	}
	seen := make(map[LaneIdentity]int, protocolsession.DefaultMaxLogicalLanes)
	for _, identity := range attempted {
		seen[identity]++
		if seen[identity] != 1 {
			t.Fatalf("full lane identity %+v was attempted %d times", identity, seen[identity])
		}
	}
	if len(seen) != protocolsession.DefaultMaxLogicalLanes {
		t.Fatalf("unique full lane identities=%d, want %d", len(seen), protocolsession.DefaultMaxLogicalLanes)
	}
	var countLeaves func(error) int
	countLeaves = func(err error) int {
		if err == nil {
			return 0
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			count := 0
			for _, child := range joined.Unwrap() {
				count += countLeaves(child)
			}
			return count
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			return countLeaves(wrapped.Unwrap())
		}
		return 1
	}
	wantLeaves := protocolsession.DefaultMaxLogicalLanes*2 + 1
	if leaves := countLeaves(runErr); leaves != wantLeaves ||
		len(runErr.Error()) > protocolsession.DefaultMaxLogicalLanes*errorBytesPerLaneBound {
		t.Fatalf("bounded error aggregation leaves=%d bytes=%d", leaves, len(runErr.Error()))
	}
	if runtime.routes.len() != 1 || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("pre-drain routes=%d active=%d", runtime.routes.len(), runtime.operations.ActiveCount())
	}
	if err := runtime.abandonOutboundOperation(
		operationID,
		transaction.route,
		transaction.generation,
	); err != nil {
		t.Fatal(err)
	}
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 ||
		runtime.operations.TombstoneCount() != 1 {
		t.Fatalf(
			"authority drain routes=%d active=%d tombstones=%d",
			runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount(),
		)
	}
	nanos.Add((protocolsession.OperationTombstoneLifetime + time.Nanosecond).Nanoseconds())
	if runtime.operations.TombstoneCount() != 1 {
		t.Fatal("transaction lease did not retain exact authority through exhaustion")
	}
	transaction.Close()
	transactionClosed = true
	if runtime.operations.TombstoneCount() != 1 {
		t.Fatal("lease release did not grant the tombstone a full post-settlement lifetime")
	}
	nanos.Add((protocolsession.OperationTombstoneLifetime + time.Nanosecond).Nanoseconds())
	if runtime.operations.TombstoneCount() != 0 {
		t.Fatal("released transaction lease retained expired authority")
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
	firstIdentity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
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
	operationContext, _ := testOutboundOperationContext(t, runtime, firstIdentity, request)
	fragments, _ := contentflow.FragmentRecord(operationID, []byte("record"))
	outbound := senderOutbound{
		runtime:    runtime,
		privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}
	if err := outbound.SendFragment(operationContext, fragments[0]); err == nil {
		t.Fatal("all-lane fragment failure unexpectedly succeeded")
	}
	<-writerDone
	if runtime.routes.len() != 1 || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("fragment failure prematurely drained route=%d active=%d", runtime.routes.len(), runtime.operations.ActiveCount())
	}
	if err := outbound.SendOperationError(operationContext, operationID, contentflow.OperationFailure{
		Scope: contentflow.BlockErrorScope, Code: contentflow.BlockCodeTimeout, Message: "Block failed",
	}); err == nil {
		t.Fatal("all-lane operation error unexpectedly succeeded")
	}
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("routes=%d active=%d tombstones=%d", runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
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
	blockedIdentity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	blocked, err := runtime.lanes.add(blockedIdentity, blockedChannel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	fallbackChannel := &countingRuntimeLaneChannel{memoryChannel: fallbackBase}
	fallbackIdentity := LaneIdentity{ID: runtime.initial.ID + 2, Epoch: 1}
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
	operationContext, _ := testOutboundOperationContext(t, runtime, blockedIdentity, request)
	fragments, _ := contentflow.FragmentRecord(operationID, []byte("record"))
	outbound := senderOutbound{runtime: runtime}
	sendContext, cancelSend := context.WithCancel(operationContext)
	sendDone := make(chan error, 1)
	go func() { sendDone <- outbound.SendFragment(sendContext, fragments[0]) }()
	<-blockedChannel.started
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonLaneRace)
	cancelMessage, _ := protocolsession.NewMessage(protocolsession.MessageCancel, &operationID, cancelBody)
	if disposition, err := (laneInboundRouter{
		runtime: runtime, identity: fallbackIdentity,
	}).RouteInbound(context.Background(), cancelMessage); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("inbound cancel disposition=%d error=%v", disposition, err)
	}
	cancelSend()
	if err := <-sendDone; !errors.Is(err, context.Canceled) || errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("cancelled transaction error=%v", err)
	}
	if fallbackChannel.sends.Load() != 0 {
		t.Fatalf("unsettled transaction migrated %d time(s)", fallbackChannel.sends.Load())
	}
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("routes=%d active=%d tombstones=%d", runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
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
	secondContext, _ := testOutboundOperationContext(t, runtime, fallbackIdentity, secondRequest)
	secondFragments, _ := contentflow.FragmentRecord(secondOperation, []byte("second-record"))
	if err := outbound.SendFragment(secondContext, secondFragments[0]); err != nil {
		t.Fatalf("second operation fragment: %v", err)
	}
	completeBody, _ := contentflow.EncodeOperationComplete(1)
	outbound.privateKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if outcome, err := outbound.SendControl(
		secondContext, protocolsession.MessageOperationComplete, secondOperation, completeBody,
	); err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("second operation outcome=%d error=%v", outcome, err)
	}
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 2 {
		t.Fatalf("post-cancel continuation routes=%d active=%d tombstones=%d", runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	close(blockedChannel.release)
	stopWriters()
	<-blockedDone
	<-fallbackDone
}

func TestSettledUnknownWithoutReplayAuthorityFailClosesSession(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	operationID := id16[protocolsession.OperationID](113)
	request, _ := protocolsession.NewMessage(
		protocolsession.MessageRequestBlocks, &operationID, []byte{0xa1, 0x00, 0x01},
	)
	operationContext, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
	transaction, err := beginOutboundTransaction(
		runtime, operationContext, operationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transaction.Run(context.Background(), func(
		selectedLane,
		protocolsession.OutboundReplayPermit,
	) (protocolsession.SendReceipt, error) {
		return protocolsession.SendReceipt{}, nil
	})
	transaction.Close()
	if outcome != protocolsession.SendOutcomeUnknown || !errors.Is(err, errOutboundReplayAuthority) {
		t.Fatalf("outcome=%d error=%v", outcome, err)
	}
	select {
	case <-runtime.ctx.Done():
	default:
		t.Fatal("missing replay authority did not terminate the session")
	}
	if runtime.routes.len() != 0 || !runtime.operations.Terminated() || runtime.operations.ActiveCount() != 0 {
		t.Fatalf("routes=%d terminal=%v active=%d", runtime.routes.len(), runtime.operations.Terminated(), runtime.operations.ActiveCount())
	}
}

func testOutboundOperationContext(
	t *testing.T,
	runtime *runtimeCore,
	identity LaneIdentity,
	request protocolsession.Message,
) (context.Context, *operationLaneRoute) {
	t.Helper()
	operationID, ok := request.OperationID()
	if !ok {
		t.Fatal("test request has no operation identity")
	}
	route := runtime.routes.reserve(operationID, identity)
	if route == nil {
		t.Fatal("operation route was not reserved")
	}
	admission, err := runtime.operations.ObserveInbound(
		protocolsession.DirectionReceiverToSender, request,
	)
	if err != nil || admission.Disposition != protocolsession.OperationDeliver || admission.Outbound.IsZero() {
		runtime.routes.releaseRoute(operationID, route)
		t.Fatalf("operation admission=%+v error=%v", admission, err)
	}
	ctx := protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
	ctx = protocolsession.WithOutboundOperationPermit(ctx, admission.Outbound)
	return bindOutboundRoute(ctx, operationID, route), route
}
