package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
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
	states := []struct {
		name           string
		prepare        func(t *testing.T, ctx context.Context, original ordinaryResumeSessionFixture)
		wantExactKind  ActiveLookupKind
		wantExactError error
	}{
		{
			name: "resumable",
			prepare: func(t *testing.T, ctx context.Context, original ordinaryResumeSessionFixture) {
				if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
					t.Fatal(err)
				}
				if err := original.authority.Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantExactKind: ActiveLookupReopened,
		},
		{
			name: "stopped share",
			prepare: func(t *testing.T, ctx context.Context, original ordinaryResumeSessionFixture) {
				if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
					t.Fatal(err)
				}
				if err := original.authority.operation.RecordShareStopped(ctx); err != nil {
					t.Fatal(err)
				}
				if err := original.authority.Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantExactError: ErrOutputShareStopped,
		},
		{
			name: "already running",
			prepare: func(*testing.T, context.Context, ordinaryResumeSessionFixture) {
				// Keep original.authority open so its operation lease stays held.
			},
			wantExactKind: ActiveLookupAlreadyRunning,
		},
		{
			name: "needs attention",
			prepare: func(t *testing.T, ctx context.Context, original ordinaryResumeSessionFixture) {
				if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
					t.Fatal(err)
				}
				if err := requireOperationAttention(original.authority.operation.lease, checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown); err != nil {
					t.Fatal(err)
				}
				if err := original.authority.Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantExactKind: ActiveLookupNeedsAttention,
		},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
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
			state.prepare(t, ctx, original)

			recordPath := filepath.Join(root, checkpointstore.ControlDirectory,
				checkpointstore.OrdinaryRegistryDirectoryV1, "operations",
				bytesToHex(original.intent.OperationID().Bytes()), "record")
			beforeRecord, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			selector, err := receivecontract.SelectOperationID(original.intent.OperationID())
			if err != nil {
				t.Fatal(err)
			}
			selectOperation := func(selection transfer.SelectionSpec) (ActiveLookup, []FilesystemOutputTrace, error) {
				t.Helper()
				var traces []FilesystemOutputTrace
				authority, err := New(Config{
					RootPath:        root,
					PlatformFactory: openOutputRuntimeTestPlatform,
					Tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
						traces = append(traces, event)
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = authority.Close() })
				if _, err := authority.BindDestination(ctx); err != nil {
					t.Fatal(err)
				}
				lookup, err := authority.SelectOperation(ctx, selection, selector)
				return lookup, traces, err
			}

			// The ID names a retained operation, but another link or selection
			// owns no saved download and must neither lock nor mutate the record.
			lookup, traces, err := selectOperation(nativeReservationTestSelection(t, 0xa1))
			if err != nil || lookup.Kind() != ActiveLookupNotFound || lookup.Operation() != nil {
				t.Fatalf("selected operation with another selection = %v, %v", lookup.Kind(), err)
			}
			if afterRecord, err := os.ReadFile(recordPath); err != nil || !bytes.Equal(beforeRecord, afterRecord) {
				t.Fatalf("non-matching selection mutated operation record on disk: %v", err)
			}
			assertSelectionMismatchTrace(t, traces, original.intent)

			lookup, _, err = selectOperation(original.intent.SelectionSpec())
			if state.wantExactError != nil {
				if !errors.Is(err, state.wantExactError) {
					t.Fatalf("exact selection = %v, %v; want %v", lookup.Kind(), err, state.wantExactError)
				}
				return
			}
			if err != nil || lookup.Kind() != state.wantExactKind {
				t.Fatalf("exact selection after mismatch = %v, %v; want %v", lookup.Kind(), err, state.wantExactKind)
			}
			if state.wantExactKind == ActiveLookupReopened {
				if _, err := lookup.authority.OpenOperation(ctx, lookup.Operation()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSelectedOperationOnLiveOnlyDestination(t *testing.T) {
	ctx := t.Context()
	liveFactory := func(path string, create bool) (outputcap.Platform, error) {
		base, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &processOnlyRuntimePlatform{Platform: base}, nil
	}
	selectLive := func(t *testing.T, root string, selection transfer.SelectionSpec, id receivecontract.OperationID) (ActiveLookup, []FilesystemOutputTrace, error) {
		t.Helper()
		var traces []FilesystemOutputTrace
		authority, err := New(Config{
			RootPath:        root,
			PlatformFactory: liveFactory,
			Tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
				traces = append(traces, event)
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })
		if mode, err := authority.BindDestination(ctx); err != nil || !mode.LiveOnly() {
			t.Fatalf("bind live-only = %v, %v", mode, err)
		}
		selector, err := receivecontract.SelectOperationID(id)
		if err != nil {
			t.Fatal(err)
		}
		lookup, err := authority.SelectOperation(ctx, selection, selector)
		return lookup, traces, err
	}

	t.Run("fresh destination reports not found", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		lookup, _, err := selectLive(t, root, nativeReservationTestSelection(t, 0xb1), receivecontract.OperationID{0xb2})
		if err != nil || lookup.Kind() != ActiveLookupNotFound {
			t.Fatalf("fresh live-only ID lookup = %v, %v", lookup.Kind(), err)
		}
	})

	t.Run("downgraded destination distinguishes matching retained operation", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		original := openOrdinaryResumeSession(t, root, 0xb3, 4)
		if err := original.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
			t.Fatal(err)
		}
		if _, err := original.transaction.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
			t.Fatal(err)
		}
		if err := original.authority.Close(); err != nil {
			t.Fatal(err)
		}

		// Wrong selection with retained ID is NotFound and emits mismatch trace.
		lookup, traces, err := selectLive(t, root, nativeReservationTestSelection(t, 0xb4), original.intent.OperationID())
		if err != nil || lookup.Kind() != ActiveLookupNotFound {
			t.Fatalf("downgraded ID with other selection = %v, %v", lookup.Kind(), err)
		}
		assertSelectionMismatchTrace(t, traces, original.intent)

		// Wrong ID with matching selection is also NotFound.
		lookup, _, err = selectLive(t, root, original.intent.SelectionSpec(), receivecontract.OperationID{0xb5})
		if err != nil || lookup.Kind() != ActiveLookupNotFound {
			t.Fatalf("downgraded unknown ID = %v, %v", lookup.Kind(), err)
		}

		// Exact ID and selection reports RecoveryUnavailable rather than a contract fault.
		lookup, _, err = selectLive(t, root, original.intent.SelectionSpec(), original.intent.OperationID())
		if err != nil || lookup.Kind() != ActiveLookupNeedsAttention || lookup.StateReason() != FilesystemOutputStateRecoveryUnavailable {
			t.Fatalf("downgraded matching ID = %v (%v), %v", lookup.Kind(), lookup.StateReason(), err)
		}
	})

	t.Run("downgraded destination isolates unrelated corrupt operations", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		healthy := openOrdinaryResumeSession(t, root, 0xb6, 4)
		corrupt := openOrdinaryResumeSession(t, root, 0xb7, 4)
		for _, fixture := range []ordinaryResumeSessionFixture{healthy, corrupt} {
			if err := fixture.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.transaction.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if err := fixture.authority.Close(); err != nil {
				t.Fatal(err)
			}
		}
		corruptRecordPath := filepath.Join(root, checkpointstore.ControlDirectory,
			checkpointstore.OrdinaryRegistryDirectoryV1, "operations",
			bytesToHex(corrupt.intent.OperationID().Bytes()), "record")
		if err := os.WriteFile(corruptRecordPath, []byte("corrupt record"), 0o600); err != nil {
			t.Fatal(err)
		}

		// A healthy ID with another selection stays NotFound despite the corrupt sibling.
		lookup, traces, err := selectLive(t, root, nativeReservationTestSelection(t, 0xb8), healthy.intent.OperationID())
		if err != nil || lookup.Kind() != ActiveLookupNotFound {
			t.Fatalf("healthy mismatched ID with corrupt sibling = %v, %v", lookup.Kind(), err)
		}
		assertSelectionMismatchTrace(t, traces, healthy.intent)

		// An absent ID also stays NotFound despite the corrupt sibling.
		lookup, _, err = selectLive(t, root, nativeReservationTestSelection(t, 0xb8), receivecontract.OperationID{0xb9})
		if err != nil || lookup.Kind() != ActiveLookupNotFound {
			t.Fatalf("absent ID with corrupt sibling = %v, %v", lookup.Kind(), err)
		}

		// Selecting the corrupt ID itself fails closed as RecoveryUnavailable.
		lookup, _, err = selectLive(t, root, corrupt.intent.SelectionSpec(), corrupt.intent.OperationID())
		if err != nil || lookup.Kind() != ActiveLookupNeedsAttention || lookup.StateReason() != FilesystemOutputStateRecoveryUnavailable {
			t.Fatalf("corrupt target ID = %v (%v), %v", lookup.Kind(), lookup.StateReason(), err)
		}
	})
}

func assertSelectionMismatchTrace(t *testing.T, traces []FilesystemOutputTrace, intent transfer.ReceiveIntent) {
	t.Helper()
	for _, event := range traces {
		if event.Operation == TraceRuntimeDecision &&
			event.RuntimeComponent == FilesystemOutputRuntimeSession &&
			event.RuntimeOperation == FilesystemOutputRuntimeSelectOperation &&
			event.RuntimeDecision == FilesystemOutputRuntimeSelectionMismatch &&
			event.ReceiveOperationID == intent.OperationID() &&
			event.ReceiveIntentDigest == intent.Digest() &&
			!event.DestinationBindingID.IsZero() &&
			!event.Failed {
			return
		}
	}
	t.Fatalf("missing selection-mismatch trace for operation %x in %+v", intent.OperationID().Bytes(), traces)
}
