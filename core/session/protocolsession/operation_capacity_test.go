package protocolsession

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestOperationAdmissionReservesEveryCompletion(t *testing.T) {
	for _, cancelLast := range []bool{false, true} {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTracked: 2}, nil)
		first, second, unknown := testOperationID(1), testOperationID(2), testOperationID(3)
		for _, id := range []OperationID{first, second} {
			if _, err := table.Observe(DirectionReceiverToSender, mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)})); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := table.Observe(DirectionSenderToReceiver, mustMessage(t, MessageOpenResults, &first, map[uint64]any{0: uint64(1)})); err != nil {
			t.Fatal(err)
		}
		if _, err := table.Observe(DirectionReceiverToSender, mustMessage(t, MessageCancel, &unknown, map[uint64]any{0: uint64(1)})); !errors.Is(err, ErrTrackedOperationBudget) {
			t.Fatalf("cancel-before-request took reserved capacity: %v", err)
		}
		direction, kind := DirectionSenderToReceiver, MessageOpenResults
		if cancelLast {
			direction, kind = DirectionReceiverToSender, MessageCancel
		}
		if _, err := table.Observe(direction, mustMessage(t, kind, &second, map[uint64]any{0: uint64(1)})); err != nil {
			t.Fatalf("reserved terminal transition: %v", err)
		}
		if table.ActiveCount() != 0 || table.TombstoneCount() != 2 || table.Terminated() {
			t.Fatal("completion did not preserve the bounded live session")
		}
	}
}

func TestOperationCapacityWaitsForExpiryWithoutTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
		id := testOperationID(4)
		_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageCancel, &id, map[uint64]any{0: uint64(1)}))
		done := make(chan error, 1)
		go func() { done <- table.WaitForCapacity(context.Background(), nil) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("capacity available before expiry: %v", err)
		default:
		}
		time.Sleep(OperationTombstoneLifetime)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestOperationCapacityWaitCancellationAndTermination(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
			id := testOperationID(5)
			_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- table.WaitForCapacity(ctx, nil) }()
			synctest.Wait()
			want := context.Canceled
			if terminal {
				want = ErrSessionTerminated
				_ = table.TerminateLocal()
			} else {
				cancel()
			}
			synctest.Wait()
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("wait = %v, want %v", err, want)
			}
		})
	}
	if err := (*OperationTable)(nil).WaitForCapacity(context.Background(), nil); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatal(err)
	}
}

func TestOperationCapacityHonorsPinnedRetentionRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
		id := testOperationID(6)
		admission, err := table.AdmitOutbound(DirectionReceiverToSender, mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)}), OutboundOperationPermit{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = table.Observe(DirectionSenderToReceiver, mustMessage(t, MessageOpenResults, &id, map[uint64]any{0: uint64(1)}))
		done := make(chan error, 1)
		go func() { done <- table.WaitForCapacity(context.Background(), nil) }()
		time.Sleep(OperationTombstoneLifetime)
		admission.pin.release()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("pin refresh did not retain the late-arrival window: %v", err)
		default:
		}
		time.Sleep(OperationTombstoneLifetime)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestWriterCapacityRaceDoesNotRetireTheLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 2}, nil)
		router, _ := NewRoleRouter(RoleReceiver, table)
		defer router.Close()
		writer, _ := NewSessionWriter(newRuntimeChannel(0), &passthroughSealer{}, router)
		first, second := testOperationID(40), testOperationID(41)
		request := func(id OperationID) Message {
			return mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)})
		}
		firstReceipt, _ := writer.TryControl(request(first))
		secondReceipt, _ := writer.TryControl(request(second))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- writer.Run(ctx) }()
		defer func() { cancel(); <-done }()
		admitted := firstReceipt.Await(ctx)
		refused := secondReceipt.Await(ctx)
		if admitted.Err != nil || refused.Admitted || refused.Outcome != SendOutcomeDropped || !IsOperationCapacityError(refused.Err) {
			t.Fatalf("admitted=%+v refused=%+v", admitted, refused)
		}
		if err := table.CancelGeneration(admitted.Generation); err != nil {
			t.Fatal(err)
		}
		retry, err := writer.TryControl(request(second))
		if err != nil {
			t.Fatal(err)
		}
		if completion := retry.Await(ctx); completion.Err != nil || !completion.Admitted || completion.Outcome != SendOutcomeTransportConfirmed {
			t.Fatalf("request could not resume on the same writer: %+v", completion)
		}
	})
}
