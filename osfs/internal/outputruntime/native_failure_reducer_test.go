package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
)

func TestReservationFailureReducersReleaseOnlyRetainedAdmissionAuthority(t *testing.T) {
	cause := errors.New("reservation failure")
	ctx := context.Background()
	if got := (&Authority{}).failPreparedAdmissionLocked(ctx, cause); got != cause {
		t.Fatalf("missing admission changed cause = %v", got)
	}

	for name, indeterminate := range map[string]bool{
		"rollback-candidate": false,
		"require-attention":  true,
	} {
		t.Run(name, func(t *testing.T) {
			durable := &checkpointstore.ActiveAdmission{}
			authority := &Authority{
				admission: &heldAdmission{durable: durable},
				stage:     authorityStageLookupHeld,
			}
			failure := cause
			if indeterminate {
				failure = errors.Join(destinationauthority.ErrReservationIndeterminate, cause)
			}
			if err := authority.failPreparedAdmissionLocked(ctx, failure); !errors.Is(err, cause) {
				t.Fatalf("durable prepared failure = %v", err)
			}
			if authority.admission != nil || authority.stage != authorityStageBound {
				t.Fatal("durable prepared failure retained admission")
			}
		})
	}

	for name, reduce := range map[string]func(*Authority) error{
		"committed": func(authority *Authority) error {
			return authority.failCommittedAdmissionLocked(ctx, nil, cause)
		},
		"reserved": func(authority *Authority) error {
			return authority.failReservedAdmissionLocked(ctx, nil, cause)
		},
	} {
		t.Run(name, func(t *testing.T) {
			authority := &Authority{
				admission: &heldAdmission{durable: &checkpointstore.ActiveAdmission{}},
				stage:     authorityStageLookupHeld,
			}
			if err := reduce(authority); !errors.Is(err, cause) {
				t.Fatalf("%s failure = %v", name, err)
			}
			if authority.admission != nil || authority.stage != authorityStageBound {
				t.Fatalf("%s failure retained admission", name)
			}
		})
	}
}

func TestPreparedReservationFailureSeparatesContractRejectionFromOwnership(t *testing.T) {
	invalid := errors.Join(destinationauthority.ErrInvalidReservation, receivecontract.ErrInvalidReceiveContract)
	for _, test := range []struct {
		name    string
		cause   error
		durable *checkpointstore.ActiveAdmission
		code    transferfault.OutputCode
	}{
		{"invalid contract", invalid, nil, transferfault.OutputContract},
		{"uncertain reservation", destinationauthority.ErrReservationIndeterminate, nil, transferfault.OutputOwnership},
		{"invalid contract with uncertain reservation", errors.Join(invalid, destinationauthority.ErrReservationIndeterminate), nil, transferfault.OutputOwnership},
		{"invalid contract with failed rollback", invalid, &checkpointstore.ActiveAdmission{}, transferfault.OutputOwnership},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := &Authority{
				admission: &heldAdmission{durable: test.durable},
				stage:     authorityStageLookupHeld,
			}
			err := authority.failPreparedAdmissionLocked(context.Background(), test.cause)
			assertOutputRuntimeFault(t, err, test.code)
			if !errors.Is(err, test.cause) {
				t.Fatalf("reservation failure lost its diagnostic cause: %v", err)
			}
			if authority.admission != nil || authority.stage != authorityStageBound {
				t.Fatal("rejected reservation retained admission authority")
			}
		})
	}
}

func TestFrozenOperationValidationQuarantinesTamperedDurableAuthority(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	authority := newNativeReservationTestAuthority(t, root.path)
	if err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0xc2)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0xc3), "tampered.bin", "tampered.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	operation.claim = destinationauthority.ReservationClaim{}
	if _, err := authority.OpenOperation(context.Background(), operation); err == nil {
		t.Fatal("tampered operation opened")
	}
	if authority.operation != nil || authority.stage != authorityStageLookupStopped ||
		!operation.closed {
		t.Fatal("tampered operation retained runtime authority")
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newNativeReservationTestAuthority(t, root.path)
	if err := reopened.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	attention, err := reopened.LookupActive(context.Background(), selection)
	if err != nil || attention.Kind() != LookupNeedsAttention {
		t.Fatalf("tampered operation lookup = (%d, %v)", attention.Kind(), err)
	}
}

func TestNativeIdentityGuardsRejectZeroInputs(t *testing.T) {
	if _, err := newReservationID(bytes.NewReader(make(
		[]byte, maximumStableIdentityGenerationAttempts*receivecontract.StableIdentityBytes,
	))); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero reservation entropy = %v", err)
	}
	if err := requireOperationAttention(nil, 0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil attention lease = %v", err)
	}
}
