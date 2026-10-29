package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputportable"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestResultRootTerminalCleanupRestartsAcrossIdentityRetirement(t *testing.T) {
	for _, event := range []checkpointmodel.OrdinaryLifecycleEvent{
		checkpointmodel.OrdinaryLifecycleComplete, checkpointmodel.OrdinaryLifecycleDiscard,
	} {
		for _, cut := range []string{"before-retirement", "after-retirement", "foreign-marker"} {
			t.Run(eventName(event)+"/"+cut, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				repository, operation, resultRoot := createIdentityCleanupOperation(t, root)
				payload := filepath.Join(resultRoot, "user-file")
				if err := os.WriteFile(payload, []byte("unrelated user data"), 0o600); err != nil {
					t.Fatal(err)
				}
				markers, err := filepath.Glob(filepath.Join(resultRoot, ".wsresume-*.identity"))
				if err != nil || len(markers) != 1 {
					t.Fatalf("result identity markers = %v, %v", markers, err)
				}
				acquired, err := repository.Acquire(ctx, operation)
				if err != nil {
					t.Fatal(err)
				}
				lease := acquired.(*NativeResumeLease)
				if _, err := lease.Transition(ctx, event, checkpointmodel.OrdinaryReasonNone); err != nil {
					t.Fatal(err)
				}
				switch cut {
				case "after-retirement":
					if err := lease.retireTopLevelIdentityLocked(); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(markers[0]); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("retired marker remains: %v", err)
					}
				case "foreign-marker":
					if err := os.WriteFile(markers[0], []byte("foreign data"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				// Stop after the terminal record is durable but before registry deletion.
				// Cleanup must not need the already-retired result-root witness.
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
				acquired, err = repository.Acquire(ctx, operation)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = acquired.Close() })
				state, cleanupErr := acquired.Cleanup(ctx)
				if cut == "foreign-marker" {
					if state != resumeauthority.CleanupPending {
						t.Fatalf("foreign identity cleanup = %v, %v", state, cleanupErr)
					}
					if actual, err := os.ReadFile(markers[0]); err != nil || string(actual) != "foreign data" {
						t.Fatalf("foreign marker changed: %q, %v", actual, err)
					}
				} else {
					if cleanupErr != nil || state != resumeauthority.CleanupComplete {
						t.Fatalf("restarted terminal cleanup = %v, %v", state, cleanupErr)
					}
					names, err := os.ReadDir(resultRoot)
					if err != nil || len(names) != 1 || names[0].Name() != "user-file" {
						t.Fatalf("cleanup retained private result state: %v, %v", names, err)
					}
				}
				if err := acquired.Close(); err != nil {
					t.Fatal(err)
				}
				if actual, err := os.ReadFile(payload); err != nil || string(actual) != "unrelated user data" {
					t.Fatalf("cleanup changed user payload: %q, %v", actual, err)
				}
				page, err := repository.Page(ctx, resumeauthority.PageCursor{}, 8)
				if err != nil {
					t.Fatal(err)
				}
				if cut == "foreign-marker" {
					if len(page.Headers()) != 1 {
						t.Fatal("foreign identity lost its retained cleanup record")
					}
				} else if len(page.Headers()) != 0 {
					t.Fatal("completed cleanup retained an operation")
				} else if _, err := os.Stat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("completed cleanup retained private namespace: %v", err)
				}
			})
		}
	}
}

func eventName(event checkpointmodel.OrdinaryLifecycleEvent) string {
	if event == checkpointmodel.OrdinaryLifecycleComplete {
		return "complete"
	}
	return "discard"
}

func createIdentityCleanupOperation(t *testing.T, root string) (*NativeResumeRepository, receivecontract.OperationID, string) {
	t.Helper()
	ctx := context.Background()
	authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, nativeReservationTestSelection(t, 0x71))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("operation has no intent")
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, outputportable.Open, nil)
	if err != nil {
		t.Fatal(err)
	}
	return repository, intent.OperationID(), filepath.Join(root, reservation.PhysicalName())
}
