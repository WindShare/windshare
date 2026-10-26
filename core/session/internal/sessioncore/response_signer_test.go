package sessioncore

import (
	"crypto/ed25519"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type testControlPreparer ed25519.PrivateKey

func (signer testControlPreparer) PrepareSenderControl(
	binding protocolsession.ControlBinding, kind protocolsession.MessageKind,
	id *protocolsession.OperationID, body []byte,
) (protocolsession.PreparedSenderControl, error) {
	return protocolsession.PrepareSenderControl(ed25519.PrivateKey(signer), binding, kind, id, body)
}
