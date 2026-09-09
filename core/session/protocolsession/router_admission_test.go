package protocolsession

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestRouterCapacityWaitDoesNotBlockCancellation(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 2}, nil)
	router, _ := NewRoleRouter(RoleSender, table)
	defer router.Close()
	ctx := context.Background()
	first, second := testOperationID(30), testOperationID(31)
	request := func(id OperationID) Message {
		return mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)})
	}
	if _, err := router.RouteInbound(ctx, request(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := router.RouteInbound(ctx, request(second)); err != nil {
		t.Fatalf("full capacity must defer the request: %v", err)
	}
	for _, id := range []OperationID{second, first} {
		if _, err := router.RouteInbound(ctx, mustMessage(t, MessageCancel, &id, map[uint64]any{0: uint64(1)})); err != nil {
			t.Fatalf("cancel during admission pressure: %v", err)
		}
	}
	if table.ActiveCount() != 0 || table.TombstoneCount() != 1 {
		t.Fatal("existing operation's cancellation was blocked by a pending request")
	}
	for range 2 {
		event, err := router.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event.generation.IsActive() {
			t.Fatal("a request cancelled during admission wait became executable")
		}
	}
	if table.TombstoneCount() != 2 || len(router.deferred) != 0 || router.deferredBytes != 0 {
		t.Fatal("deferred cancellation did not settle its retained identity")
	}
}

func TestRouterDeferredRequestPromotesAtRetentionExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
		router, _ := NewRoleRouter(RoleSender, table)
		defer router.Close()
		first, second := testOperationID(32), testOperationID(33)
		_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageCancel, &first, map[uint64]any{0: uint64(1)}))
		request := mustMessage(t, MessageOpenRevisions, &second, map[uint64]any{0: uint64(1)})
		if _, err := router.RouteInbound(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if disposition, err := router.RouteInbound(context.Background(), request); err != nil || disposition != OperationDrop {
			t.Fatalf("pending exact replay = %v, %v", disposition, err)
		}
		done := make(chan RouteEvent, 1)
		go func() {
			event, err := router.Next(context.Background())
			if err != nil {
				t.Error(err)
			}
			done <- event
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("deferred request acquired an unreserved slot")
		default:
		}
		time.Sleep(OperationTombstoneLifetime)
		synctest.Wait()
		event := <-done
		if event.operationID != second || !event.generation.IsActive() || table.Terminated() {
			t.Fatal("request did not resume in the same session")
		}
	})
}

func TestRouterDeferredAdmissionIsBoundedAndCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
		router, _ := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 1, DataFrames: 1})
		id := testOperationID(34)
		_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)}))
		id = testOperationID(35)
		_, err := router.RouteInbound(context.Background(), mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)}))
		if err != nil {
			t.Fatal(err)
		}
		id = testOperationID(36)
		if _, err := router.RouteInbound(context.Background(), mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)})); !errors.Is(err, ErrRouterControlFull) {
			t.Fatalf("unbounded deferred ingress: %v", err)
		}
		done := make(chan error, 1)
		go func() { _, err := router.Next(context.Background()); done <- err }()
		synctest.Wait()
		router.Close()
		synctest.Wait()
		if err := <-done; !errors.Is(err, ErrSessionTerminated) {
			t.Fatal(err)
		}
		if len(router.deferred) != 0 || router.deferredFrames != 0 {
			t.Fatal("close retained deferred admission")
		}
	})
}

func TestFullDeferredInboxReservesCancellation(t *testing.T) {
	for _, kinds := range [][]MessageKind{{MessageOpenRevisions, MessageCancel}, {MessageCancel, MessageOpenRevisions}} {
		synctest.Test(t, func(t *testing.T) {
			table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
			router, _ := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 1, DataFrames: 1})
			defer router.Close()
			first, second := testOperationID(37), testOperationID(38)
			_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageCancel, &first, map[uint64]any{0: uint64(1)}))
			for _, kind := range kinds {
				if _, err := router.RouteInbound(context.Background(), mustMessage(t, kind, &second, map[uint64]any{0: uint64(1)})); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(OperationTombstoneLifetime)
			event, err := router.Next(context.Background())
			if err != nil || event.message.Kind() != MessageCancel || event.generation.IsActive() {
				t.Fatalf("pending cancellation = %+v, %v", event, err)
			}
			if router.deferredFrames != 0 || router.deferredBytes != 0 || router.deferredCancelBytes != 0 {
				t.Fatal("inbox accounting leaked")
			}
		})
	}
}

func TestDeferredAdmissionPrecedesFreshArrivals(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	table, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTracked: 1}, func() time.Time { return now })
	router, _ := NewRoleRouter(RoleSender, table)
	defer router.Close()
	first, second, third := testOperationID(50), testOperationID(51), testOperationID(52)
	_, _ = table.Observe(DirectionReceiverToSender, mustMessage(t, MessageCancel, &first, map[uint64]any{0: uint64(1)}))
	request := func(id OperationID) Message {
		return mustMessage(t, MessageOpenRevisions, &id, map[uint64]any{0: uint64(1)})
	}
	if _, err := router.RouteInbound(context.Background(), request(second)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(OperationTombstoneLifetime)
	if _, err := router.RouteInbound(context.Background(), request(third)); err != nil {
		t.Fatal(err)
	}
	event, err := router.Next(context.Background())
	if err != nil || event.operationID != second {
		t.Fatalf("fresh arrival overtook deferred work: %+v, %v", event, err)
	}
	_ = table.TerminateLocal()
	if disposition, err := router.RouteInbound(context.Background(), request(third)); err != nil || disposition != OperationDrop {
		t.Fatalf("terminal session retained new pending work: %v, %v", disposition, err)
	}
}
