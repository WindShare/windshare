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
	op, _, _, release, err := runtime.operations.AdmitOutboundRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	if err := runtime.operations.TerminateLocal(); err != nil {
		t.Fatal(err)
	}
	if transitioned, err := runtime.reconcileLocalCancel(op.Ref()); err != nil || transitioned || runtime.Err() != nil {
		t.Fatalf("retired RPC failed cleanup: transitioned=%v error=%v runtime=%v", transitioned, err, runtime.Err())
	}
	if runtime.ctx.Err() != nil || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 0 {
		t.Fatal("late RPC cleanup changed runtime lifecycle or retained authority")
	}
	// Session termination must not make a malformed cancellation capability valid.
	if _, err := runtime.reconcileLocalCancel(protocolsession.OperationRef{}); !errors.Is(err, protocolsession.ErrInvalidOperationID) {
		t.Fatalf("invalid cancellation error=%v", err)
	}
	if !errors.Is(runtime.Err(), protocolsession.ErrInvalidOperationID) || runtime.ctx.Err() == nil {
		t.Fatal("invalid cancellation no longer fails the runtime")
	}
}

func TestRPCFailedBeginCancellationUsesReservedCapacity(t *testing.T) {
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2}, nil,
	)
	firstID := id16[protocolsession.OperationID](82)
	first, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &firstID, []byte{0xa1, 0x00, 0x01},
	)
	firstOp, _, _, firstRelease, err := runtime.operations.AdmitOutboundRequest(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstRelease(false)
	if transitioned, err := runtime.operations.CancelOperation(firstOp.Ref()); err != nil || !transitioned {
		t.Fatalf("cancel first: transitioned=%v err=%v", transitioned, err)
	}
	secondID := id16[protocolsession.OperationID](83)
	second, _ := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &secondID, []byte{0xa1, 0x00, 0x02},
	)
	secondOp, _, _, secondRelease, err := runtime.operations.AdmitOutboundRequest(second, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease(false)
	if transitioned, err := runtime.reconcileLocalCancel(secondOp.Ref()); err != nil || !transitioned {
		t.Fatalf("cleanup transitioned=%v error=%v", transitioned, err)
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatal("reserved cancellation ended the session")
	default:
	}
	if runtime.operations.Terminated() || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 2 {
		t.Fatalf("terminal=%v active=%d tombstones=%d", runtime.operations.Terminated(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}
