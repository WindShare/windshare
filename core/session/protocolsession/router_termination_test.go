package protocolsession

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type nextBarrierContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *nextBarrierContext) Err() error {
	ctx.once.Do(func() { close(ctx.entered) })
	return ctx.Context.Err()
}

func TestRoleRouterLocalTerminationClosesSharedOperationState(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRoleRouter(RoleReceiver, table)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.TerminateLocal(); err != nil {
		t.Fatalf("terminate local router: %v", err)
	}
	if !table.Terminated() {
		t.Fatal("local router termination left operation admission open")
	}
	if err := (*RoleRouter)(nil).TerminateLocal(); !errors.Is(err, ErrNilRuntimeDependency) {
		t.Fatalf("nil local router termination = %v", err)
	}
}

func TestRoleRouterNextDoesNotDispatchBacklogAfterLifetimeCancellation(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	router, _ := NewRoleRouter(RoleSender, table)
	operationID := testOperationID(211)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	if disposition, err := router.RouteInbound(context.Background(), request); err != nil || disposition != OperationDeliver {
		t.Fatalf("queue request: disposition=%d error=%v", disposition, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := router.Next(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backlog dequeue error=%v", err)
	}
	router.Close()
	if table.ActiveCount() != 0 || table.TombstoneCount() != 0 {
		t.Fatalf("router close retained active=%d tombstones=%d", table.ActiveCount(), table.TombstoneCount())
	}
}

func TestRoleRouterCloseWakesNextAndDrainsFullQueues(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	router, _ := NewRoleRouterWithLimits(RoleReceiver, table, RouterLimits{ControlFrames: 1, DataFrames: 1})

	operationID := testOperationID(212)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	blockAdmission, err := router.AdmitOutbound(request, OutboundOperationPermit{})
	if err != nil {
		t.Fatalf("admit block request: %v", err)
	}
	blockAdmission.pin.release()
	fragment := mustFragmentMessage(t, operationID, 1)
	if disposition, err := router.RouteInbound(context.Background(), fragment); err != nil || disposition != OperationDeliver {
		t.Fatalf("fill data queue: disposition=%d error=%v", disposition, err)
	}
	catalogID := testOperationID(213)
	catalogRequest := mustMessage(t, MessageListChildren, &catalogID, map[uint64]any{0: uint64(1)})
	catalogAdmission, err := router.AdmitOutbound(catalogRequest, OutboundOperationPermit{})
	if err != nil {
		t.Fatalf("admit catalog request: %v", err)
	}
	catalogAdmission.pin.release()
	progress := mustMessage(t, MessageScanProgress, &catalogID, map[uint64]any{0: uint64(1)})
	if disposition, err := router.RouteInbound(context.Background(), progress); err != nil || disposition != OperationDeliver {
		t.Fatalf("fill control queue: disposition=%d error=%v", disposition, err)
	}
	router.Close()
	if len(router.control) != 0 || len(router.data) != 0 || len(router.pendingData) != 0 {
		t.Fatalf("closed router retained control=%d data=%d pending=%d", len(router.control), len(router.data), len(router.pendingData))
	}
	if _, err := router.Next(context.Background()); !errors.Is(err, ErrSessionTerminated) {
		t.Fatalf("Next after close error=%v", err)
	}

	wakeTable, _ := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 1}, nil)
	wakeRouter, _ := NewRoleRouter(RoleSender, wakeTable)
	barrier := &nextBarrierContext{Context: context.Background(), entered: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := wakeRouter.Next(barrier)
		result <- err
	}()
	<-barrier.entered
	wakeRouter.Close()
	if err := <-result; !errors.Is(err, ErrSessionTerminated) {
		t.Fatalf("blocked Next close error=%v", err)
	}
}
