package sessioncore

import (
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

func TestProtocolErrorTracingLeavesDisabledInboundHotPathUnbound(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	id := protocolsession.OperationID(0x51)
	message, _ := protocolsession.NewMessage(protocolsession.MessageOperationError, &id, []byte{0xf6})
	call := newOperationCall(id, protocolsession.MessageOpenRevisions, time.Time{}, 0, false, false)
	if err := call.enqueue(operationResponse{message: message}); err != nil {
		t.Fatal(err)
	}
	if call.traceCause != sessionobs.ProtocolOperationCauseNone {
		t.Fatal("disabled trace retained failure")
	}
	if runtime.protocolOperationTracingEnabled() {
		t.Fatal("disabled trace unexpectedly enabled")
	}
}
