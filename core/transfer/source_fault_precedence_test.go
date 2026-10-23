package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestSourceBoundaryKeepsEvidencePrecedenceAcrossCleanupCauses(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  fault.Fault
	}{
		{"invalidation with protocol and close", errors.Join(content.ErrRevisionDrift, ErrBlockIdentity, ErrBrokerClosed),
			mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated)},
		{"revision change with close", errors.Join(content.ErrRevisionStale, ErrBrokerClosed),
			mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged)},
		{"identity with transport cleanup", errors.Join(ErrBlockIdentity, protocolsession.ErrWriterStopped),
			mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol)},
		{"broker invalidation with missing source", errors.Join(ErrBlockInvalidated, content.ErrRevisionNotFound),
			mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedFault(normalizeSourceBoundary(context.Background(), tc.cause))
			if got != tc.want {
				t.Fatalf("fault = %v, want %v", got, tc.want)
			}
		})
	}
}
