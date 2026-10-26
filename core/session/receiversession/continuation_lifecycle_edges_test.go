package receiversession

import (
	"context"
	"testing"
)

func TestReceiverPeerOperationReportsGenerationContinuationBudget(t *testing.T) {
	if maximum, ok := (*PeerOperation)(nil).MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("nil peer continuation budget=%d,%t", maximum, ok)
	}
	if maximum, ok := (&PeerOperation{}).MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("unconstructed peer continuation budget=%d,%t", maximum, ok)
	}

	t.Run("local terminal retains admitted budget", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 140)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		termination := fixture.operation.Terminate(context.Background())
		assertReceiverPeerTermination(
			t,
			fixture.operation,
			termination,
			PeerTerminalAuthorityLocal,
			PeerProvenanceLocalExplicitStop,
			PeerTerminalOperationOnly,
			PeerProvenanceLocalExplicitStop,
		)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
	})

	t.Run("runtime shutdown retains admitted budget", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 141)
		if err := fixture.runtime.AddFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}
		stopTestCore(fixture.runtime)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		termination := fixture.operation.Terminate(context.Background())
		assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
	})

	t.Run("call close retains admission snapshot", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 142)
		if maximum, ok := fixture.call.ContinuationLimit(); !ok || maximum != 4 {
			t.Fatalf("live call continuation snapshot=%d,%t", maximum, ok)
		}
		fixture.call.Close()
		if maximum, ok := fixture.call.ContinuationLimit(); !ok || maximum != 4 {
			t.Fatalf("closed call continuation snapshot=%d,%t", maximum, ok)
		}
	})
}

func assertReceiverPeerContinuationBudget(
	t *testing.T,
	operation *PeerOperation,
	want int,
) {
	t.Helper()
	if maximum, ok := operation.MaximumContinuations(); !ok || maximum != want {
		t.Fatalf("peer continuation budget=%d,%t want=%d,true", maximum, ok, want)
	}
}
