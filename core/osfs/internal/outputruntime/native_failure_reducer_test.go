package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReservationFailureReducersReleaseOnlyRetainedAdmissionAuthority(t *testing.T) {
	cause := errors.New("reservation failure")
	ctx := context.Background()
	if got := (&Authority{}).failPreparedAdmissionLocked(ctx, cause); got != cause {
		t.Fatalf("missing admission changed cause = %v", got)
	}

	authorityRef, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat(
		[]byte{0xc1}, receivecontract.AuthorityRefBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	volatile, err := newVolatileReservationClaimer(authorityRef)
	if err != nil {
		t.Fatal(err)
	}
	authority := &Authority{
		admission: &heldAdmission{volatile: volatile},
		stage:     authorityStageLookupHeld,
	}
	if err := authority.failPreparedAdmissionLocked(ctx, cause); !errors.Is(err, cause) {
		t.Fatalf("volatile prepared failure = %v", err)
	}
	if authority.admission != nil || authority.stage != authorityStageBound || !volatile.closed {
		t.Fatal("volatile admission remained authoritative after reservation failure")
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

func TestFrozenOperationValidationQuarantinesTamperedDurableAuthority(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	authority := newNativeReservationTestAuthority(t, root.path)
	if _, err := authority.BindDestination(context.Background()); err != nil {
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
	if _, err := reopened.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	attention, err := reopened.LookupActive(context.Background(), selection)
	if err != nil || attention.Kind() != ActiveLookupNeedsAttention {
		t.Fatalf("tampered operation lookup = (%d, %v)", attention.Kind(), err)
	}
}

func TestFrozenLiveOperationRejectsLostVolatileClaim(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	factory := func(path string, create bool) (outputcap.Platform, error) {
		base, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &liveOnlyRuntimePlatform{Platform: base}, nil
	}
	authority, err := New(Config{RootPath: root.path, PlatformFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0xc4)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0xc5), "live-tampered.bin", "live-tampered.bin",
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
	operation.volatile.closed = true
	if _, err := authority.OpenOperation(context.Background(), operation); err == nil {
		t.Fatal("live operation without volatile name claim opened")
	}
	if !operation.closed || authority.stage != authorityStageLookupStopped {
		t.Fatal("invalid live operation retained authority")
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFacadeAndIdentityGuardsRejectUnboundedInputs(t *testing.T) {
	var authority *Authority
	if _, err := authority.reserveNativeDirectTree(
		context.Background(), transfer.SelectionSpec{}, receivecontract.ArtifactSpec{},
	); !errors.Is(err, transfer.ErrInvalidReceiveIntent) {
		t.Fatalf("nil facade reserve = %v", err)
	}
	authority = &Authority{}
	selection := nativeReservationTestSelection(t, 0xc6)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0xc7), "not-catalog.bin", "not-catalog.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.reserveNativeDirectTree(
		context.Background(), selection, artifact,
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("bounded artifact through catalog facade = %v", err)
	}
	if _, err := newReservationID(bytes.NewReader(make(
		[]byte, maximumStableIdentityGenerationAttempts*receivecontract.StableIdentityBytes,
	))); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero reservation entropy = %v", err)
	}
	if err := requireOperationAttention(nil, 0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil attention lease = %v", err)
	}
}
