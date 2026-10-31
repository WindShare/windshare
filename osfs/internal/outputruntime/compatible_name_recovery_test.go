package outputruntime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputnames"
	"github.com/windshare/windshare/osfs/internal/outputportable"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestPortableSingleFileNameSurvivesResumeAndTerminalCleanup(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(fmt.Sprintf("discard=%t", discard), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			const name = "report?.pdf"
			first := openNamedResumeFile(t, root, 0x31, 4, false, name, outputportable.Open, nil)
			if err := first.transaction.WriteRange(ctx, 0, []byte("ab")); err != nil {
				t.Fatal(err)
			}
			if _, err := first.transaction.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			pauseOrdinaryResumeFixture(t, first)

			resumed := openNamedResumeFile(t, root, 0x31, 4, true, name, outputportable.Open, nil)
			t.Cleanup(func() { _ = resumed.authority.Close() })
			if resumed.finalPath != first.finalPath || !resumed.intent.EqualCanonical(first.intent) {
				t.Fatal("resume changed the physical reservation")
			}
			ranges := resumed.durable.Ranges().Ranges()
			if len(ranges) != 1 || ranges[0] != (content.Range{Offset: 0, End: 2}) {
				t.Fatalf("durable bytes = %+v", ranges)
			}
			if err := resumed.transaction.WriteRange(ctx, 2, []byte("cd")); err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.transaction.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			// A foreign report-shaped file must not be replaced by cleanup.
			operation := hex.EncodeToString(first.intent.OperationID().Bytes())
			foreign := filepath.Join(root, fmt.Sprintf("windshare-names-%s-1-0.json", operation))
			if err := os.WriteFile(foreign, []byte("foreign report"), 0o600); err != nil {
				t.Fatal(err)
			}
			if discard {
				if _, err := resumed.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
					t.Fatal(err)
				}
				if err := resumed.authority.Close(); err != nil {
					t.Fatal(err)
				}
				repository, err := NewNativeResumeRepository(root, outputportable.Open, nil)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := repository.Acquire(ctx, first.intent.OperationID())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lease.Transition(ctx, checkpointmodel.OrdinaryLifecycleDiscard, checkpointmodel.OrdinaryReasonNone); err != nil {
					t.Fatal(err)
				}
				if state, err := lease.Cleanup(ctx); err != nil || state != resumeauthority.CleanupComplete {
					t.Fatalf("discard = %v, %v", state, err)
				}
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := resumed.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
					t.Fatal(err)
				}
				if err := resumed.authority.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if actual, err := os.ReadFile(resumed.finalPath); err != nil || string(actual) != "abcd" {
				t.Fatalf("result = %q, %v", actual, err)
			}
			if actual, err := os.ReadFile(foreign); err != nil || string(actual) != "foreign report" {
				t.Fatal("foreign report was changed")
			}
			if runtime.GOOS == "windows" {
				if filepath.Base(resumed.finalPath) == name || !strings.HasSuffix(resumed.finalPath, ".pdf") {
					t.Fatal("single-file repair lost its extension")
				}
				assertNativeNameReport(t, filepath.Join(root, fmt.Sprintf("windshare-names-%s-1-1.json", operation)), name, filepath.Base(resumed.finalPath), 0)
			}
		})
	}
}

func TestMappedNamesCleanupRestartsAfterResultIdentityRetirement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(incrementalTestIdentity16[catalog.DirectoryID](0x71), "folder?")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, nativeReservationTestSelection(t, 0x72))
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	session, err := authority.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		operation.names, err = outputnames.New(operation.intent.OperationID().Bytes(), namesRoot(operation.topLevel), operation.nameStore,
			refusingNameNamespace{outputnames.GuardedNamespace{Acquire: operation.topLevel.AcquirePublicOperationGuard}}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	physical, err := operation.names.Bind("folder?/file?.txt")
	if err != nil {
		t.Fatal(err)
	}
	rootName := operation.topLevel.ReservedEntry().PhysicalName()
	result := filepath.Join(root, rootName, physical)
	if err := os.WriteFile(result, []byte("published content"), 0o600); err != nil {
		t.Fatal(err)
	}
	operationID := operation.intent.OperationID()
	if _, err := session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := NewNativeResumeRepository(root, outputportable.Open, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	if _, err := lease.Transition(ctx, checkpointmodel.OrdinaryLifecycleDiscard, checkpointmodel.OrdinaryReasonNone); err != nil {
		t.Fatal(err)
	}
	if err := lease.cleanupNamesLocked(); err != nil {
		t.Fatal(err)
	}
	if err := lease.retireTopLevelIdentityLocked(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	acquired, err = repository.Acquire(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acquired.Close() })
	if state, err := acquired.Cleanup(ctx); err != nil || state != resumeauthority.CleanupComplete {
		t.Fatalf("restarted cleanup = %v, %v", state, err)
	}
	if err := acquired.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(result); err != nil || string(actual) != "published content" {
		t.Fatalf("cleanup changed result: %q, %v", actual, err)
	}
	report := filepath.Join(root, fmt.Sprintf("windshare-names-%s-1-0.json", hex.EncodeToString(operationID.Bytes())))
	assertNativeNameReport(t, report, "folder?", rootName, 1)
}

func assertNativeNameReport(t *testing.T, path, originalRoot, savedRoot string, entries int) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		OriginalRoot string
		SavedRoot    string
		Entries      []struct{ OriginalPath, SavedPath string }
	}
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.OriginalRoot != originalRoot || report.SavedRoot != savedRoot || len(report.Entries) != entries {
		t.Fatalf("incomplete name report: %s", encoded)
	}
}

type refusingNameNamespace struct{ outputnames.Namespace }

func (namespace refusingNameNamespace) InspectName(parent, name string) (outputcap.EntryKind, error) {
	if strings.Contains(name, "?") {
		return 0, outputcap.ErrNameRejected
	}
	return namespace.Namespace.InspectName(parent, name)
}
