package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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

func TestResumeStateAuthorityW6ProjectsAttentionAndDiagnosticReferences(t *testing.T) {
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
	authority, err := newResumeStateAuthority(&publicOrdinaryStore{
		header: header, lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != ResumeStateListNeedsAttention ||
		len(summaries) != 1 ||
		summaries[0].State() != ResumeOperationNeedsAttention ||
		summaries[0].NeedsAttentionReason() != "operation-ownership-unknown" ||
		len(summaries[0].Items()) != 1 ||
		summaries[0].Items()[0].DiagnosticReference() != "checkpoint-bad-mac" ||
		summaries[0].Items()[0].BlockReason() != ResumeItemBlockCheckpointInvalid {
		t.Fatalf("attention inventory = (%+v, %v)", inventory, err)
	}
}

func TestResumeStateAuthorityW6PropagatesPageAcquireAndSnapshotFailures(t *testing.T) {
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
			authority, err := newResumeStateAuthority(test.store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := authority.ListResumeState(context.Background()); !errors.Is(err, sentinel) {
				t.Fatalf("list error = %v", err)
			}
		})
	}

	nilLease, _ := newResumeStateAuthority(&publicOrdinaryStore{header: fixture.header})
	if _, err := nilLease.ListResumeState(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil lease error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	authority, _ := newResumeStateAuthority(&publicOrdinaryStore{header: fixture.header})
	if _, err := authority.ListResumeState(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list error = %v", err)
	}
	var nilContext context.Context
	if _, err := authority.ListResumeState(nilContext); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil-context list error = %v", err)
	}
}

func TestResumeStateAuthorityW6PreservesCleanupUncertaintyAndCloseErrors(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0x91)
	cleanupErr := errors.New("cleanup durability uncertain")
	closeErr := errors.New("lease close failed")
	lease := &publicOrdinaryLease{
		header: fixture.header, cleanup: resumeauthority.CleanupPending,
		cleanupErr: cleanupErr, closeErr: closeErr,
	}
	authority, _ := newResumeStateAuthority(&publicOrdinaryStore{
		header: fixture.header, lease: lease,
	})
	summary, err := authority.Discard(context.Background(), fixture.operation)
	if summary.State() != ResumeOperationCleanupPending ||
		!errors.Is(err, cleanupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("cleanup-pending discard = (%+v, %v)", summary, err)
	}

	invalidFixture := newPublicOrdinaryFixture(t, 0xa1)
	invalidLease := &publicOrdinaryLease{
		header: invalidFixture.header, cleanupErr: cleanupErr,
	}
	invalidAuthority, _ := newResumeStateAuthority(&publicOrdinaryStore{
		header: invalidFixture.header, lease: invalidLease,
	})
	if _, err := invalidAuthority.Discard(
		context.Background(), invalidFixture.operation,
	); !errors.Is(err, ErrResumeStateContract) || !errors.Is(err, cleanupErr) {
		t.Fatalf("invalid cleanup outcome error = %v", err)
	}
}

func TestResumeStateAuthorityW6RejectsDiscardTransitionAndInputFaults(t *testing.T) {
	fixture := newPublicOrdinaryFixture(t, 0xb1)
	transitionErr := errors.New("registry transition failed")
	lease := &publicOrdinaryLease{
		header: fixture.header, cleanup: resumeauthority.CleanupComplete,
		transitionErr: transitionErr,
	}
	authority, _ := newResumeStateAuthority(&publicOrdinaryStore{
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
	); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil-context discard error = %v", err)
	}
	if _, err := authority.Discard(
		context.Background(), receivecontract.OperationID{},
	); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("zero-operation discard error = %v", err)
	}
}
