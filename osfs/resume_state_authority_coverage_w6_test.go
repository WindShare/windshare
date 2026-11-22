package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func publicAttentionHeader(
	t *testing.T,
	fixture publicOrdinaryFixture,
	reason checkpointmodel.OrdinaryClosedReason,
) resumeauthority.Header {
	t.Helper()
	lease := &publicOrdinaryLease{header: fixture.header}
	header, err := lease.Transition(
		context.Background(),
		checkpointmodel.OrdinaryLifecycleRequireAttention,
		reason,
	)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func TestRecoveryAuthorityProjectsAttentionAndDiagnosticReferences(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x71)
	header := publicAttentionHeader(
		t, fixture, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
	)
	reference, err := resumeauthority.NewBlockedReference("checkpoint-bad-mac")
	if err != nil {
		t.Fatal(err)
	}
	lease := &publicOrdinaryLease{
		header: header, items: []resumeauthority.Item{reference},
		cleanup: resumeauthority.CleanupPending,
	}
	authority := newRecoveryAuthorityWithStore(&publicOrdinaryStore{
		header: header, lease: lease,
	})
	inventory, err := authority.List(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != RecoveryListNeedsAttention ||
		len(summaries) != 1 ||
		summaries[0].State() != RecoveryOperationNeedsAttention ||
		summaries[0].NeedsAttentionReason() != RecoveryReasonOperationOwnership ||
		len(summaries[0].Items()) != 1 ||
		summaries[0].Items()[0].Reference() != "checkpoint-bad-mac" ||
		summaries[0].Items()[0].BlockReason() != RecoveryItemBlockCheckpointInvalid {
		t.Fatalf("attention inventory = (%+v, %v)", inventory, err)
	}
}

func TestRecoveryAuthorityPropagatesPageAcquireAndSnapshotFailures(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x81)
	sentinel := errors.New("resume boundary failed")
	tests := []struct {
		name  string
		store *publicOrdinaryStore
	}{
		{
			name: "page",
			store: &publicOrdinaryStore{
				header: fixture.header, pageErr: sentinel,
			},
		},
		{
			name: "acquire",
			store: &publicOrdinaryStore{
				header: fixture.header, acquireErr: sentinel,
			},
		},
		{
			name: "snapshot",
			store: &publicOrdinaryStore{
				header: fixture.header,
				lease: &publicOrdinaryLease{
					header: fixture.header, snapshotErr: sentinel,
				},
			},
		},
		{
			name: "close",
			store: &publicOrdinaryStore{
				header: fixture.header,
				lease: &publicOrdinaryLease{
					header: fixture.header, closeErr: sentinel,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := newRecoveryAuthorityWithStore(test.store)
			if _, err := authority.List(context.Background()); !errors.Is(err, sentinel) {
				t.Fatalf("list error = %v", err)
			}
		})
	}

	nilLease := newRecoveryAuthorityWithStore(&publicOrdinaryStore{header: fixture.header})
	if _, err := nilLease.List(context.Background()); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("nil lease error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	authority := newRecoveryAuthorityWithStore(&publicOrdinaryStore{header: fixture.header})
	if _, err := authority.List(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list error = %v", err)
	}
	var nilContext context.Context
	if _, err := authority.List(nilContext); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("nil-context list error = %v", err)
	}
}

func TestRecoveryAuthorityPreservesCleanupUncertaintyAndCloseErrors(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x91)
	cleanupErr := errors.New("cleanup durability uncertain")
	closeErr := errors.New("lease close failed")
	lease := &publicOrdinaryLease{
		header: fixture.header, cleanup: resumeauthority.CleanupPending,
		cleanupErr: cleanupErr, closeErr: closeErr,
	}
	authority := newRecoveryAuthorityWithStore(&publicOrdinaryStore{
		header: fixture.header, lease: lease,
	})
	summary, err := authority.Discard(context.Background(), fixture.operation)
	if summary.State() != RecoveryOperationCleanupPending ||
		!errors.Is(err, cleanupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("cleanup-pending discard = (%+v, %v)", summary, err)
	}

	invalidFixture := newPublicOrdinaryFixture(t, 0xa1)
	invalidLease := &publicOrdinaryLease{
		header: invalidFixture.header, cleanupErr: cleanupErr,
	}
	invalidAuthority := newRecoveryAuthorityWithStore(&publicOrdinaryStore{
		header: invalidFixture.header, lease: invalidLease,
	})
	if _, err := invalidAuthority.Discard(
		context.Background(), invalidFixture.operation,
	); !errors.Is(err, ErrRecoveryContract) || !errors.Is(err, cleanupErr) {
		t.Fatalf("invalid cleanup outcome error = %v", err)
	}
}

func TestRecoveryAuthorityRejectsDiscardTransitionAndInputFaults(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0xb1)
	transitionErr := errors.New("registry transition failed")
	lease := &publicOrdinaryLease{
		header: fixture.header, cleanup: resumeauthority.CleanupComplete,
		transitionErr: transitionErr,
	}
	authority := newRecoveryAuthorityWithStore(&publicOrdinaryStore{
		header: fixture.header, lease: lease,
	})
	if _, err := authority.Discard(
		context.Background(), fixture.operation,
	); !errors.Is(err, transitionErr) {
		t.Fatalf("transition error = %v", err)
	}
	var nilContext context.Context
	if _, err := authority.Discard(
		nilContext, fixture.operation,
	); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("nil-context discard error = %v", err)
	}
	if _, err := authority.Discard(
		context.Background(), receivecontract.OperationID{},
	); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("zero-operation discard error = %v", err)
	}
}
