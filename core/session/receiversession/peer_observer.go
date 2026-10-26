package receiversession

import (
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// This adapter keeps protocol violation delivery private to the operation owner.
type peerOperationObserver struct{ operation *PeerOperation }

func (observer peerOperationObserver) BindTerminalOwner(owner *sessioncore.TerminalOwner) {
	observer.operation.protocolTerminal = owner
}
func (observer peerOperationObserver) ObserveAuthenticatedOperationViolation(violation protocolsession.AuthenticatedOperationViolation) {
	observer.operation.observeAuthenticatedOperationViolation(violation)
}
