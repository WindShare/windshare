package protocolsession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

type settlementTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *settlementTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *settlementTestClock) Set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

type settlementGateChannel struct {
	entered chan struct{}
	release chan struct{}
	sendErr error
	once    sync.Once
}

func newSettlementGateChannel(sendErr error) *settlementGateChannel {
	return &settlementGateChannel{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		sendErr: sendErr,
	}
}

func (channel *settlementGateChannel) Send(context.Context, framechannel.Frame) error {
	channel.once.Do(func() { close(channel.entered) })
	<-channel.release
	return channel.sendErr
}

func (channel *settlementGateChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

func (*settlementGateChannel) Recv() <-chan framechannel.Frame  { return nil }
func (*settlementGateChannel) State() framechannel.ChannelState { return framechannel.Open }
func (*settlementGateChannel) Close() error                     { return nil }

func TestSessionWriterSettlementLeaseAnchorsTombstoneAfterPhysicalSend(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		sendErr     error
		wantOutcome SendOutcome
	}{
		{name: "delivered", wantOutcome: SendOutcomeDelivered},
		{name: "unknown", sendErr: errors.New("transport accepted before error"), wantOutcome: SendOutcomeUnknown},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			initialTime := time.Unix(1_900_000_000, 0)
			clock := &settlementTestClock{now: initialTime}
			operations, err := NewOperationTable(
				OperationLimits{MaxActive: 2, MaxTombstones: 2},
				clock.Now,
			)
			if err != nil {
				t.Fatal(err)
			}
			router, err := NewRoleRouter(RoleSender, operations)
			if err != nil {
				t.Fatal(err)
			}
			operationID := testOperationID(231)
			request := mustMessage(t, MessageOpenRevisions, &operationID, map[uint64]any{0: uint64(1)})
			admission, err := operations.ObserveInbound(DirectionReceiverToSender, request)
			if err != nil {
				t.Fatal(err)
			}
			transactionLease, err := admission.Outbound.AcquireLease()
			if err != nil {
				t.Fatal(err)
			}

			channel := newSettlementGateChannel(testCase.sendErr)
			writer, err := NewSessionWriter(channel, &passthroughSealer{}, router)
			if err != nil {
				t.Fatal(err)
			}
			preparedFinal := mustPreparedControl(t, MessageOpenResults, &operationID)
			canonicalFinal, err := preparedFinal.build(0)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := writer.TryAuthorizedSenderControl(
				preparedFinal,
				admission.Outbound,
			)
			if err != nil {
				t.Fatal(err)
			}
			receipt.ReleaseLeaseOnSettlement(transactionLease)
			runContext, stopWriter := context.WithCancel(context.Background())
			defer stopWriter()
			runDone := make(chan error, 1)
			go func() { runDone <- writer.Run(runContext) }()
			<-channel.entered

			// The physical send remains unsettled beyond the original tombstone
			// window. Its transferred transaction lease must prevent same-ID B
			// from being admitted while the transport still owns the frame.
			clock.Set(initialTime.Add(OperationTombstoneLifetime + time.Second))
			if disposition, replayErr := operations.Observe(
				DirectionReceiverToSender,
				request,
			); disposition != OperationDrop || replayErr != nil {
				t.Fatalf("same ID admitted during physical send: disposition=%d err=%v", disposition, replayErr)
			}

			close(channel.release)
			completion := receipt.Await(context.Background())
			if completion.Outcome != testCase.wantOutcome || !completion.Settled ||
				completion.Replay.IsZero() || !errors.Is(completion.Err, testCase.sendErr) {
				t.Fatalf("physical completion = %+v", completion)
			}
			if testCase.sendErr == nil {
				stopWriter()
				if runErr := <-runDone; !errors.Is(runErr, context.Canceled) {
					t.Fatalf("delivered writer stop = %v", runErr)
				}
			} else if runErr := <-runDone; !errors.Is(runErr, testCase.sendErr) {
				t.Fatalf("unknown writer stop = %v", runErr)
			}
			settledAt := clock.Now()

			for replayIndex := 0; replayIndex < 2; replayIndex++ {
				clock.Set(settledAt.Add(
					time.Duration(replayIndex+1) * (OperationTombstoneLifetime / 3),
				))
				replay, replayErr := operations.AcceptOutboundReplay(
					DirectionSenderToReceiver,
					canonicalFinal,
					completion.Replay,
				)
				if replayErr != nil || replay.Disposition != OperationDeliver {
					t.Fatalf("post-settlement replay %d=%v err=%v", replayIndex, replay.Disposition, replayErr)
				}
				replay.pin.release()
			}

			clock.Set(settledAt.Add(OperationTombstoneLifetime - time.Nanosecond))
			if disposition, replayErr := operations.Observe(
				DirectionReceiverToSender,
				request,
			); disposition != OperationDrop || replayErr != nil {
				t.Fatalf("same ID admitted before settlement TTL: disposition=%d err=%v", disposition, replayErr)
			}
			clock.Set(settledAt.Add(OperationTombstoneLifetime + time.Nanosecond))
			next, err := operations.ObserveInbound(DirectionReceiverToSender, request)
			if err != nil || next.Disposition != OperationDeliver ||
				next.Generation.Same(admission.Generation) {
				t.Fatalf("same ID B after settlement TTL = %+v, %v", next, err)
			}
		})
	}
}

func TestSessionWriterDropsQueuedStaleGenerationBeforePolicyMutation(t *testing.T) {
	initialTime := time.Unix(1_910_000_000, 0)
	clock := &settlementTestClock{now: initialTime}
	operations, err := NewOperationTable(
		OperationLimits{MaxActive: 2, MaxTombstones: 2},
		clock.Now,
	)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRoleRouter(RoleSender, operations)
	if err != nil {
		t.Fatal(err)
	}
	channel := newRuntimeChannel(0)
	writer, err := NewSessionWriter(channel, &passthroughSealer{}, router)
	if err != nil {
		t.Fatal(err)
	}

	operationID := testOperationID(232)
	request := mustMessage(t, MessageOpenRevisions, &operationID, map[uint64]any{0: uint64(1)})
	generationA, err := operations.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil {
		t.Fatal(err)
	}
	staleReceipt, err := writer.TryAuthorizedSenderControl(
		mustPreparedControl(t, MessageOpenResults, &operationID),
		generationA.Outbound,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := operations.CancelGeneration(generationA.Generation); err != nil {
		t.Fatal(err)
	}
	clock.Set(initialTime.Add(OperationTombstoneLifetime + time.Nanosecond))
	generationB, err := operations.ObserveInbound(DirectionReceiverToSender, request)
	if err != nil || generationB.Disposition != OperationDeliver ||
		generationB.Generation.Same(generationA.Generation) {
		t.Fatalf("same-ID generation B = %+v, %v", generationB, err)
	}

	runContext, stopWriter := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- writer.Run(runContext) }()
	staleCompletion := staleReceipt.Await(context.Background())
	if !staleCompletion.Settled || staleCompletion.Admitted ||
		staleCompletion.Outcome != SendOutcomeDropped || staleCompletion.Err != nil {
		t.Fatalf("stale queued A completion = %+v", staleCompletion)
	}
	channel.mu.Lock()
	staleFrames := len(channel.sent)
	channel.mu.Unlock()
	if staleFrames != 0 || !generationB.Generation.IsActive() ||
		operations.ActiveCount() != 1 || operations.TombstoneCount() != 0 {
		t.Fatalf(
			"stale A mutated B: frames=%d B-active=%v active=%d tombstones=%d",
			staleFrames,
			generationB.Generation.IsActive(),
			operations.ActiveCount(),
			operations.TombstoneCount(),
		)
	}

	currentReceipt, err := writer.TryAuthorizedSenderControl(
		mustPreparedControl(t, MessageOpenResults, &operationID),
		generationB.Outbound,
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, waitErr := currentReceipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
		t.Fatalf("generation B send = %d, %v", outcome, waitErr)
	}
	channel.mu.Lock()
	currentFrames := len(channel.sent)
	channel.mu.Unlock()
	if currentFrames != 1 || generationB.Generation.IsActive() ||
		!generationB.Generation.IsCurrent() || generationA.Generation.IsCurrent() {
		t.Fatalf(
			"generation B final state: frames=%d A-current=%v B-active=%v B-current=%v",
			currentFrames,
			generationA.Generation.IsCurrent(),
			generationB.Generation.IsActive(),
			generationB.Generation.IsCurrent(),
		)
	}

	stopWriter()
	if runErr := <-runDone; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("writer stop = %v", runErr)
	}
}
