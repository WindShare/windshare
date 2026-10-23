package failurestate

import (
	"errors"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

// ContentFault classifies source evidence independently of transport teardown.
// Each caller supplies its own terminal errors after admitting this evidence.
func ContentFault(err error) (fault.Fault, bool) {
	switch {
	case errors.Is(err, content.ErrRevisionDrift):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), true
	case errors.Is(err, content.ErrRevisionStale), errors.Is(err, content.ErrSourceDrift):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged), true
	case errors.Is(err, content.ErrRevisionNotFound), errors.Is(err, content.ErrRevisionUnreadable),
		errors.Is(err, content.ErrUnsupportedStability):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourcePermanent), true
	case errors.Is(err, content.ErrQuotaExceeded), errors.Is(err, content.ErrLeaseExpired),
		errors.Is(err, content.ErrInvalidLease):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceUnavailable), true
	default:
		return fault.Fault{}, false
	}
}

func SessionTerminal(err error) bool {
	return errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) ||
		errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped)
}

func mustSourceFault(scope fault.Scope, code fault.SourceCode) fault.Fault {
	value, _ := fault.NewSource(scope, code)
	return value
}
