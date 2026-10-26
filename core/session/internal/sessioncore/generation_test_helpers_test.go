package sessioncore

import (
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func operationMessageForTest(
	t *testing.T,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.NewMessage(kind, &operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
