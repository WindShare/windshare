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

func TestSelectedOperationWithAnotherSelectionIsNotFound(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "resumable"
		if stopped {
			name = "stopped share"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root := newRuntimeTestRootSpec(t).path
			original := openOrdinaryResumeSession(t, root, 0x91, 4)
			t.Cleanup(func() { _ = original.authority.Close() })
			if err := original.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
				t.Fatal(err)
			}
			if _, err := original.transaction.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if stopped {
				if err := original.authority.operation.RecordShareStopped(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := original.authority.Close(); err != nil {
				t.Fatal(err)
			}
			selector, err := receivecontract.SelectOperationID(original.intent.OperationID())
			if err != nil {
				t.Fatal(err)
			}
			selectOperation := func(selection transfer.SelectionSpec) (ActiveLookup, error) {
				t.Helper()
				authority := newNativeReservationTestAuthority(t, root)
				t.Cleanup(func() { _ = authority.Close() })
				if _, err := authority.BindDestination(ctx); err != nil {
					t.Fatal(err)
				}
				return authority.SelectOperation(ctx, selection, selector)
			}

			// The ID names a retained operation, but another link or selection
			// owns no saved download; the operation stays exactly retryable.
			lookup, err := selectOperation(nativeReservationTestSelection(t, 0xa1))
			if err != nil || lookup.Kind() != ActiveLookupNotFound || lookup.Operation() != nil {
				t.Fatalf("selected operation with another selection = %v, %v", lookup.Kind(), err)
			}
			lookup, err = selectOperation(original.intent.SelectionSpec())
			if stopped {
				if !errors.Is(err, ErrOutputShareStopped) {
					t.Fatalf("exact stopped selection = %v, %v", lookup.Kind(), err)
				}
				return
			}
			if err != nil || lookup.Kind() != ActiveLookupReopened {
				t.Fatalf("exact selection after mismatch = %v, %v", lookup.Kind(), err)
			}
			if _, err := lookup.authority.OpenOperation(ctx, lookup.Operation()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
