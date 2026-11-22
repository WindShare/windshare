package fault

import (
	"errors"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// ClassifyContent maps authenticated content-layer errors to file-local source
// faults independently of transport teardown.
func ClassifyContent(err error) (Fault, bool) {
	switch {
	case errors.Is(err, content.ErrRevisionDrift):
		value, _ := NewSource(ScopeFileLocal, SourceRevisionInvalidated)
		return value, true
	case errors.Is(err, content.ErrRevisionStale), errors.Is(err, content.ErrSourceDrift):
		value, _ := NewSource(ScopeFileLocal, SourceRevisionChanged)
		return value, true
	case errors.Is(err, content.ErrRevisionNotFound), errors.Is(err, content.ErrRevisionUnreadable),
		errors.Is(err, content.ErrUnsupportedStability):
		value, _ := NewSource(ScopeFileLocal, SourcePermanent)
		return value, true
	case errors.Is(err, content.ErrRevisionReopened), errors.Is(err, content.ErrQuotaExceeded),
		errors.Is(err, content.ErrLeaseExpired), errors.Is(err, content.ErrInvalidLease):
		value, _ := NewSource(ScopeFileLocal, SourceUnavailable)
		return value, true
	default:
		return Fault{}, false
	}
}

// IsSessionTerminal reports whether err represents terminal protocol-session teardown.
func IsSessionTerminal(err error) bool {
	return errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) ||
		errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped)
}
