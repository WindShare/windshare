package lanes

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestLaneFailureKeepsSourceEvidenceBeforeRetirement(t *testing.T) {
	expected, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceRevisionInvalidated)
	for _, terminal := range []error{ErrClosed, protocolsession.ErrWriterStopped} {
		combined := errors.Join(content.ErrRevisionDrift, terminal)
		got := fault.FaultOf(normalizeSourceBoundary(context.Background(), combined))
		if got != expected {
			t.Fatalf("lane failure = %v, want %v", got, expected)
		}
	}
	transport, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionTransport)
	if got := fault.FaultOf(normalizeSourceBoundary(context.Background(), ErrClosed)); got != transport {
		t.Fatalf("bare lane closure = %v, want %v", got, transport)
	}
}
