package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestMissingSelectedOperationCannotAuthorizeCreation(t *testing.T) {
	ctx := context.Background()
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x61)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x63), "selected.bin", "selected.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	authority := newNativeReservationTestAuthority(t, root.path)
	if mode, err := authority.BindDestination(ctx); err != nil || !mode.Resumable() {
		t.Fatalf("bind = %v, %v", mode, err)
	}
	selector, err := receivecontract.SelectOperationID(receivecontract.OperationID{0x65})
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, selector)
	if err != nil || lookup.Kind() != ActiveLookupNotFound || lookup.Operation() != nil {
		t.Fatalf("missing selection = %v, %v", lookup.Kind(), err)
	}
	if _, err := authority.CreateOperation(ctx, lookup, artifact); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("missing selection authorized a replacement download: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	next := newNativeReservationTestAuthority(t, root.path)
	defer next.Close()
	if _, err := next.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err = next.LookupActive(ctx, selection)
	if err != nil || lookup.Kind() != ActiveLookupCreateReady {
		t.Fatalf("failed selection retained admission: %v, %v", lookup.Kind(), err)
	}
	if _, err := next.CreateOperation(ctx, lookup, artifact); err != nil {
		t.Fatal(err)
	}
}
