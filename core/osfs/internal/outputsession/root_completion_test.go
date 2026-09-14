package outputsession

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestTreeCompletionDistinguishesUnadmittedRootFromUnsettledOutput(t *testing.T) {
	tests := []struct {
		name     string
		admit    bool
		finalize bool
		outcome  transfer.DirectTreeOutcome
		want     transfer.DirectTreeSettlementKind
	}{
		{"unadmitted partial", false, false, transfer.DirectTreeOutcomePartial, transfer.DirectTreeSettlementPartial},
		{"unadmitted success", false, false, transfer.DirectTreeOutcomeSuccess, transfer.DirectTreeSettlementFailed},
		{"unsettled partial", true, false, transfer.DirectTreeOutcomePartial, transfer.DirectTreeSettlementFailed},
		{"unsettled success", true, false, transfer.DirectTreeOutcomeSuccess, transfer.DirectTreeSettlementFailed},
		{"empty directory partial", true, true, transfer.DirectTreeOutcomePartial, transfer.DirectTreeSettlementPartial},
		{"empty directory success", true, true, transfer.DirectTreeOutcomeSuccess, transfer.DirectTreeSettlementSuccess},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestFixture(t, nil)
			ctx := context.Background()
			if test.admit {
				root := fixture.admitRoot(ctx)
				if test.finalize {
					if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
						t.Fatal(err)
					}
				}
			}
			settlement, err := fixture.session.FinalizeTree(ctx, test.outcome)
			if settlement.Kind() != test.want {
				t.Fatalf("tree settlement = (%d, %v), want %d", settlement.Kind(), err, test.want)
			}
			if test.want == transfer.DirectTreeSettlementFailed {
				if !errors.Is(err, ErrConflictingSettlement) {
					t.Fatalf("unsettled output lost contract failure: %v", err)
				}
			} else if err != nil {
				t.Fatalf("known output ownership became a failure: %v", err)
			}
			cached, cachedErr := fixture.session.FinalizeTree(ctx, test.outcome)
			if cached != settlement || cachedErr != err {
				t.Fatalf("repeated close changed settlement: (%v, %v)", cached, cachedErr)
			}
			fixture.resources.mu.Lock()
			calls := fixture.resources.calls
			fixture.resources.mu.Unlock()
			if calls != 1 {
				t.Fatalf("resource releases = %d, want 1", calls)
			}
		})
	}
}
