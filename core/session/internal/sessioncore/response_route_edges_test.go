package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestResponseWithoutLaneAndMalformedFailureReleaseRoute(t *testing.T) {
	sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	identity := sender.initial
	outbound := NewResponses(sender, ResponseConfig{})
	fragments, err := contentflow.FragmentRecord(id16[protocolsession.OperationID](77), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	sender.lanes.detach(identity)
	if err := outbound.SendFragment(context.Background(), protocolsession.Operation{}, fragments[0]); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("fragment without operation authority error = %v", err)
	}
	invalidFailureID := id16[protocolsession.OperationID](76)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &invalidFailureID, []byte{0xa0})
	admission, err := sender.operations.ObserveInbound(protocolsession.DirectionReceiverToSender, identity, request)
	if err != nil || admission.op.IsZero() {
		t.Fatalf("admit request: %v", err)
	}
	if err := outbound.SendOperationError(context.Background(), admission.op, contentflow.OperationFailure{}); err == nil {
		t.Fatal("invalid operation failure was sent")
	}
	if admission.op.IsActive() || sender.operations.ActiveCount() != 0 || sender.operations.TombstoneCount() != 1 {
		t.Fatalf("invalid operation failure retained active operation: active=%d tombstones=%d",
			sender.operations.ActiveCount(), sender.operations.TombstoneCount())
	}
}
