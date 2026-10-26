package sessioncore

import (
	"errors"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestRPCLocalCancelAfterSessionTermination(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	operationID := id16[protocolsession.OperationID](84)
	request, err := protocolsession.NewMessage(protocolsession.MessageListChildren, &operationID, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := runtime.router.AdmitOutbound(request, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.TerminateLocal(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileLocalCancel(admission.Generation); err != nil || runtime.Err() != nil {
		t.Fatalf("retired RPC failed cleanup: error=%v runtime=%v", err, runtime.Err())
	}
	if runtime.ctx.Err() != nil || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 0 {
		t.Fatal("late RPC cleanup changed runtime lifecycle or retained authority")
	}
	// Session termination must not make a malformed cancellation capability valid.
	if err := runtime.reconcileLocalCancel(protocolsession.OperationGeneration{}); !errors.Is(err, protocolsession.ErrInvalidOperationID) {
		t.Fatalf("invalid cancellation error=%v", err)
	}
	if !errors.Is(runtime.Err(), protocolsession.ErrInvalidOperationID) || runtime.ctx.Err() == nil {
		t.Fatal("invalid cancellation no longer fails the runtime")
	}
}

func TestRPCFailedBeginCancellationUsesReservedCapacity(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	runtime.operations = operations
	runtime.router = router
	firstID := id16[protocolsession.OperationID](82)
	first, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &firstID, []byte{0xa1, 0x00, 0x01},
	)
	firstAdmission, err := router.AdmitOutbound(first, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := operations.CancelGeneration(firstAdmission.Generation); err != nil {
		t.Fatal(err)
	}
	secondID := id16[protocolsession.OperationID](83)
	second, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &secondID, []byte{0xa1, 0x00, 0x02},
	)
	secondAdmission, err := router.AdmitOutbound(second, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileLocalCancel(secondAdmission.Generation); err != nil {
		t.Fatalf("cleanup error=%v", err)
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatal("reserved cancellation ended the session")
	default:
	}
	if operations.Terminated() || operations.ActiveCount() != 0 || operations.TombstoneCount() != 2 {
		t.Fatalf("terminal=%v active=%d tombstones=%d", operations.Terminated(), operations.ActiveCount(), operations.TombstoneCount())
	}
}
