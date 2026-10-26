package sendersession

import (
	sessionobs "github.com/windshare/windshare/core/session/observation"
)

type senderTerminalObservers struct {
	send    sessionobs.SenderTerminalSendObserver
	session sessionobs.SenderSessionTerminalObserver
}

func newSenderTerminalObservers(
	send sessionobs.SenderTerminalSendObserver,
	session sessionobs.SenderSessionTerminalObserver,
) *senderTerminalObservers {
	if send == nil && session == nil {
		return nil
	}
	return &senderTerminalObservers{send: send, session: session}
}

func (observers *senderTerminalObservers) sendObserver() sessionobs.SenderTerminalSendObserver {
	if observers == nil {
		return nil
	}
	return observers.send
}

func (observers *senderTerminalObservers) sessionObserver() sessionobs.SenderSessionTerminalObserver {
	if observers == nil {
		return nil
	}
	return observers.session
}
