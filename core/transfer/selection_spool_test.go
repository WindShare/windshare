package transfer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectionSpoolExternallySortsWidePlanAndCleansItsPrivateRoot(t *testing.T) {
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](1))
	if err != nil {
		t.Fatal(err)
	}
	root := spool.root
	t.Cleanup(func() { _ = spool.Close() })
	parent := transferID[catalog.DirectoryID](2)
	generation := transferID[catalog.DirectoryGeneration](3)
	const records = 300
	inserted := make([]string, 0, records)
	for index := records - 1; index >= 0; index-- {
		path := wideSelectionPath(index)
		inserted = append(inserted, path)
		if err := spool.claim(selectionSpoolNodeID(t, index+10)); err != nil {
			t.Fatal(err)
		}
		if err := spool.appendFile(plannedFile{
			file:             transferID[catalog.FileID](byte(index%250 + 1)),
			path:             path,
			parentDirectory:  parent,
			parentGeneration: generation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if spool.rawBytes <= selectionPlanSortMemoryBytes {
		t.Fatalf("raw plan bytes = %d, want external-sort spill beyond %d", spool.rawBytes, selectionPlanSortMemoryBytes)
	}
	if err := spool.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(inserted)
	slices.Sort(want)
	got := make([]string, 0, records)
	if err := spool.VisitFiles(func(file plannedFile) error {
		got = append(got, file.path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatal("externally sorted selection plan is not in canonical path order")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "plan-run-") || strings.HasPrefix(entry.Name(), "claim-run-") {
			t.Fatalf("temporary sort run survived freeze: %q", entry.Name())
		}
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selection spool root survived close: %v", err)
	}
}

func TestSelectionSpoolClosesMergeRunsWhenDuplicatePathRejectsFreeze(t *testing.T) {
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](4))
	if err != nil {
		t.Fatal(err)
	}
	root := spool.root
	parent := transferID[catalog.DirectoryID](5)
	generation := transferID[catalog.DirectoryGeneration](6)
	appendFile := func(index int, path string) {
		t.Helper()
		if err := spool.appendFile(plannedFile{
			file:             transferID[catalog.FileID](byte(index%250 + 1)),
			path:             path,
			parentDirectory:  parent,
			parentGeneration: generation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	duplicate := wideSelectionPath(0)
	appendFile(0, duplicate)
	for index := 1; index <= 300; index++ {
		appendFile(index, wideSelectionPath(index))
	}
	appendFile(301, duplicate)
	if spool.rawBytes <= selectionPlanSortMemoryBytes {
		t.Fatal("duplicate-path fixture did not cross an external-sort run boundary")
	}
	if err := spool.Freeze(context.Background()); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("duplicate path freeze error = %v", err)
	}
	// Merge failures used to leave run handles open, which made private-root
	// cleanup fail specifically on Windows.
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed selection spool root survived close: %v", err)
	}
}

func TestSelectionSpoolRejectsDuplicateNodeClaims(t *testing.T) {
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](7))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	claim := selectionSpoolNodeID(t, 1)
	if err := spool.claim(claim); err != nil {
		t.Fatal(err)
	}
	if err := spool.claim(claim); err != nil {
		t.Fatal(err)
	}
	if err := spool.Freeze(context.Background()); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("duplicate identity claim freeze error = %v", err)
	}
}

func TestSelectionSpoolFailsClosedAtIdentityClaimBudget(t *testing.T) {
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](8))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	spool.claimCount = maximumSelectionClaims
	if err := spool.claim(selectionSpoolNodeID(t, 2)); !errors.Is(err, ErrSelectionPlanBudget) ||
		!isJobTerminalError(err) || isSessionFailure(err) {
		t.Fatalf("identity claim budget error = %v", err)
	}
}

func TestSelectionSpoolCheckpointOwnsPlanAndIdentityClaims(t *testing.T) {
	if _, err := newSelectionSpool(catalog.ShareInstance{}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("zero-share spool = %v", err)
	}
	collision := filepath.Join(t.TempDir(), "already-created")
	file, err := os.Create(collision)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := secureSelectionFile(collision); err == nil {
		t.Fatal("exclusive selection file creation accepted an existing path")
	}

	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](9))
	if err != nil {
		t.Fatal(err)
	}
	root := spool.root
	t.Cleanup(func() { _ = spool.Close() })
	if err := spool.claim(catalog.NodeID{}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("zero identity claim = %v", err)
	}
	directory := plannedDirectory{
		directory:  transferID[catalog.DirectoryID](10),
		generation: transferID[catalog.DirectoryGeneration](11),
		path:       "directory",
	}
	reference, err := spool.appendDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.requireDirectory(selectionDirectoryReference{}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("invalid directory reference = %v", err)
	}
	if err := spool.requireDirectory(reference); err != nil {
		t.Fatal(err)
	}
	if err := spool.requireDirectory(reference); err != nil {
		t.Fatalf("idempotent directory requirement = %v", err)
	}
	baseFile := plannedFile{
		file:             transferID[catalog.FileID](12),
		path:             "directory/base",
		parentDirectory:  directory.directory,
		parentGeneration: directory.generation,
	}
	if err := spool.claim(selectionSpoolNodeID(t, 12)); err != nil {
		t.Fatal(err)
	}
	if err := spool.appendFile(baseFile); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := spool.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.claim(selectionSpoolNodeID(t, 13)); err != nil {
		t.Fatal(err)
	}
	if err := spool.appendFile(plannedFile{
		file:             transferID[catalog.FileID](13),
		path:             "directory/discarded",
		parentDirectory:  directory.directory,
		parentGeneration: directory.generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := spool.rollback(checkpoint); err != nil {
		t.Fatal(err)
	}
	if spool.DirectoryCount() != 1 || spool.FileCount() != 1 || spool.claimCount != 1 {
		t.Fatalf("rolled-back counts = directories %d, files %d, claims %d", spool.DirectoryCount(), spool.FileCount(), spool.claimCount)
	}
	// Reusing the rolled-back identity proves the claims file and its logical
	// count share the same transaction boundary as the output plan.
	if err := spool.claim(selectionSpoolNodeID(t, 13)); err != nil {
		t.Fatal(err)
	}
	invalidCheckpoint := checkpoint
	invalidCheckpoint.rawOffset++
	if err := spool.rollback(invalidCheckpoint); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("misaligned rollback = %v", err)
	}
	previousBytes := spool.rawBytes
	spool.rawBytes = maximumSelectionPlanBytes
	if err := spool.appendFile(baseFile); !errors.Is(err, ErrSelectionPlanBudget) || !isJobTerminalError(err) {
		t.Fatalf("raw plan budget = %v", err)
	}
	spool.rawBytes = previousBytes
	if err := spool.appendFile(plannedFile{path: "not-canonical/../file"}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("invalid path append = %v", err)
	}

	if err := spool.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spool.DirectoryCount() != 1 || spool.FileCount() != 1 {
		t.Fatalf("terminal counts = directories %d, files %d", spool.DirectoryCount(), spool.FileCount())
	}
	var records []selectionPlanRecord
	if err := spool.VisitRecords(func(record selectionPlanRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].path != "directory" || records[1].path != baseFile.path {
		t.Fatalf("terminal plan = %#v", records)
	}
	var reverseDirectories []string
	if err := spool.VisitDirectoriesReverse(func(directory plannedDirectory) error {
		reverseDirectories = append(reverseDirectories, directory.path)
		return nil
	}); err != nil || !slices.Equal(reverseDirectories, []string{"directory"}) {
		t.Fatalf("reverse directories = %v, err %v", reverseDirectories, err)
	}
	visitorFailure := errors.New("visitor stopped")
	if err := spool.VisitFiles(func(plannedFile) error { return visitorFailure }); !errors.Is(err, visitorFailure) {
		t.Fatalf("visitor error = %v", err)
	}
	if err := spool.claim(selectionSpoolNodeID(t, 14)); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("claim after freeze = %v", err)
	}
	if _, err := spool.checkpoint(); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("checkpoint after freeze = %v", err)
	}
	if err := spool.rollback(checkpoint); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("rollback after freeze = %v", err)
	}
	if err := spool.requireDirectory(reference); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("require after freeze = %v", err)
	}
	if err := spool.Freeze(context.Background()); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("second freeze = %v", err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed spool root = %v", err)
	}
}

func TestSelectionSpoolCancellationFailsClosedBeforeSorting(t *testing.T) {
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](15))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := spool.Freeze(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled freeze = %v", err)
	}
	if _, err := spool.checkpoint(); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("checkpoint after cancelled freeze = %v", err)
	}
}

func TestSelectionSpoolFailsClosedOnDiscoveryFileCorruption(t *testing.T) {
	parent := transferID[catalog.DirectoryID](30)
	generation := transferID[catalog.DirectoryGeneration](31)
	file := plannedFile{
		file:             transferID[catalog.FileID](32),
		path:             "file",
		parentDirectory:  parent,
		parentGeneration: generation,
	}
	directory := plannedDirectory{
		directory:  transferID[catalog.DirectoryID](33),
		generation: transferID[catalog.DirectoryGeneration](34),
		path:       "directory",
	}

	t.Run("claim write", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 35)
		if err := spool.claims.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.claim(selectionSpoolNodeID(t, 35)); err == nil {
			t.Fatal("closed claim file accepted an identity")
		}
	})
	t.Run("directory marker read", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 36)
		reference, err := spool.appendDirectory(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.requireDirectory(reference); err == nil {
			t.Fatal("closed plan file supplied a directory marker")
		}
	})
	t.Run("directory marker value", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 37)
		reference, err := spool.appendDirectory(directory)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.raw.WriteAt([]byte{2}, reference.activeOffset); err != nil {
			t.Fatal(err)
		}
		if err := spool.requireDirectory(reference); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("corrupt directory marker = %v", err)
		}
	})
	t.Run("directory marker write", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 38)
		reference, err := spool.appendDirectory(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		spool.raw, err = os.Open(spool.rawPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.requireDirectory(reference); err == nil {
			t.Fatal("read-only plan accepted a directory-marker write")
		}
	})
	t.Run("record encoding", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 39)
		if _, err := spool.appendDirectory(plannedDirectory{path: "directory"}); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("invalid directory record = %v", err)
		}
	})
	t.Run("record seek", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 40)
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.appendFile(file); err == nil {
			t.Fatal("closed plan accepted an append")
		}
	})
	t.Run("record write", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 41)
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		spool.raw, err = os.Open(spool.rawPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.appendFile(file); err == nil {
			t.Fatal("read-only plan accepted an append")
		}
	})
	t.Run("checkpoint raw seek", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 42)
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.checkpoint(); err == nil {
			t.Fatal("checkpoint ignored a closed plan")
		}
	})
	t.Run("checkpoint claim seek", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 43)
		if err := spool.claims.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.checkpoint(); err == nil {
			t.Fatal("checkpoint ignored closed claims")
		}
	})
	t.Run("checkpoint position", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 44)
		if err := spool.appendFile(file); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.raw.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.checkpoint(); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("mispositioned checkpoint = %v", err)
		}
	})
	t.Run("rollback plan", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 45)
		checkpoint, err := spool.checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.appendFile(file); err != nil {
			t.Fatal(err)
		}
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.rollback(checkpoint); err == nil {
			t.Fatal("rollback ignored a closed plan")
		}
	})
	t.Run("rollback claims", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 46)
		checkpoint, err := spool.checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.claim(selectionSpoolNodeID(t, 46)); err != nil {
			t.Fatal(err)
		}
		if err := spool.claims.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.rollback(checkpoint); err == nil {
			t.Fatal("rollback ignored closed claims")
		}
	})
	t.Run("sync plan", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 47)
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.Freeze(context.Background()); err == nil {
			t.Fatal("freeze ignored a closed plan")
		}
	})
	t.Run("sync claims", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 48)
		if err := spool.claims.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.Freeze(context.Background()); err == nil {
			t.Fatal("freeze ignored closed claims")
		}
	})
	t.Run("sort input", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 49)
		if err := spool.raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.sortTerminalPlan(context.Background()); err == nil {
			t.Fatal("sort ignored a closed discovery plan")
		}
	})
	t.Run("claim input", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 50)
		if err := spool.claims.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.rejectDuplicateClaims(context.Background()); err == nil {
			t.Fatal("identity sort ignored closed claims")
		}
	})
	t.Run("partial claim", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 51)
		if _, err := spool.claims.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := spool.rejectDuplicateClaims(context.Background()); err == nil {
			t.Fatal("partial identity claim was accepted")
		}
	})
	t.Run("cancelled claim merge", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 52)
		if err := spool.claim(selectionSpoolNodeID(t, 52)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := spool.rejectDuplicateClaims(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled identity merge = %v", err)
		}
	})
	t.Run("terminal path collision", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 53)
		if err := spool.appendFile(file); err != nil {
			t.Fatal(err)
		}
		terminal, err := os.Create(spool.planPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := terminal.Close(); err != nil {
			t.Fatal(err)
		}
		if err := spool.sortTerminalPlan(context.Background()); err == nil {
			t.Fatal("existing terminal plan was overwritten")
		}
	})
	t.Run("cancelled plan sort", func(t *testing.T) {
		spool := newTestSelectionSpool(t, 54)
		if err := spool.appendFile(file); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := spool.sortTerminalPlan(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled plan sort = %v", err)
		}
	})
}

func TestSelectionSpoolVisitorsRejectUnavailableTerminalPlans(t *testing.T) {
	unfrozen := newTestSelectionSpool(t, 55)
	if err := unfrozen.VisitRecords(func(selectionPlanRecord) error { return nil }); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("unfrozen visit = %v", err)
	}

	missing := newTestSelectionSpool(t, 56)
	if err := missing.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing.planPath); err != nil {
		t.Fatal(err)
	}
	if err := missing.VisitRecords(func(selectionPlanRecord) error { return nil }); err == nil {
		t.Fatal("missing terminal plan was accepted")
	}

	corruptForward := newTestSelectionSpool(t, 57)
	if err := corruptForward.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(corruptForward.planPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := corruptForward.VisitRecords(func(selectionPlanRecord) error { return nil }); err == nil {
		t.Fatal("corrupt forward plan was accepted")
	}

	corruptReverse := newTestSelectionSpool(t, 58)
	if err := corruptReverse.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	file, err = os.OpenFile(corruptReverse.planPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := corruptReverse.VisitDirectoriesReverse(func(plannedDirectory) error { return nil }); err == nil {
		t.Fatal("corrupt reverse plan was accepted")
	}
}

func TestSelectionSpoolSortingAndCleanupFailureBoundaries(t *testing.T) {
	parent := transferID[catalog.DirectoryID](60)
	generation := transferID[catalog.DirectoryGeneration](61)
	file := plannedFile{
		file:             transferID[catalog.FileID](62),
		path:             "file",
		parentDirectory:  parent,
		parentGeneration: generation,
	}

	inactive := newTestSelectionSpool(t, 63)
	if _, err := inactive.appendDirectory(plannedDirectory{
		directory: transferID[catalog.DirectoryID](63), generation: generation, path: "inactive",
	}); err != nil {
		t.Fatal(err)
	}
	if err := inactive.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	var inactiveRecords int
	if err := inactive.VisitRecords(func(selectionPlanRecord) error {
		inactiveRecords++
		return nil
	}); err != nil || inactiveRecords != 0 {
		t.Fatalf("inactive directory records = %d, err %v", inactiveRecords, err)
	}

	removeFailure := newTestSelectionSpool(t, 64)
	if err := removeFailure.appendFile(file); err != nil {
		t.Fatal(err)
	}
	removeFailure.rawPath = string([]byte{0})
	if err := removeFailure.Freeze(context.Background()); err == nil {
		t.Fatal("discovery-file removal failure was ignored")
	}

	planRunFailure := newTestSelectionSpool(t, 65)
	if err := planRunFailure.appendFile(file); err != nil {
		t.Fatal(err)
	}
	planRoot := planRunFailure.root
	planRunFailure.root = filepath.Join(planRoot, "missing", "root")
	if err := planRunFailure.sortTerminalPlan(context.Background()); err == nil {
		t.Fatal("missing plan-run root was accepted")
	}
	planRunFailure.root = planRoot

	claimRunFailure := newTestSelectionSpool(t, 66)
	if err := claimRunFailure.claim(selectionSpoolNodeID(t, 66)); err != nil {
		t.Fatal(err)
	}
	claimRoot := claimRunFailure.root
	claimRunFailure.root = filepath.Join(claimRoot, "missing", "root")
	if err := claimRunFailure.rejectDuplicateClaims(context.Background()); err == nil {
		t.Fatal("missing claim-run root was accepted")
	}
	claimRunFailure.root = claimRoot

	closed, err := newSelectionSpool(transferID[catalog.ShareInstance](67))
	if err != nil {
		t.Fatal(err)
	}
	actualRoot := closed.root
	closed.root = string([]byte{0})
	if err := closed.Close(); err == nil {
		t.Fatal("private-root removal failure was ignored")
	}
	if err := os.RemoveAll(actualRoot); err != nil {
		t.Fatal(err)
	}

	corruptDiscovery := newTestSelectionSpool(t, 69)
	if _, err := corruptDiscovery.raw.Write([]byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := corruptDiscovery.sortTerminalPlan(context.Background()); err == nil {
		t.Fatal("truncated discovery plan was accepted by terminal sorting")
	}

	duplicatePath := newTestSelectionSpool(t, 70)
	duplicateDirectory := plannedDirectory{
		directory: transferID[catalog.DirectoryID](70), generation: generation, path: "same",
	}
	reference, err := duplicatePath.appendDirectory(duplicateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicatePath.requireDirectory(reference); err != nil {
		t.Fatal(err)
	}
	if err := duplicatePath.appendFile(plannedFile{
		file:             transferID[catalog.FileID](71),
		path:             "same",
		parentDirectory:  parent,
		parentGeneration: generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := duplicatePath.sortTerminalPlan(context.Background()); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("cross-kind duplicate path = %v", err)
	}
}

func TestSelectionSpoolDetectsDuplicateIdentityAcrossExternalRuns(t *testing.T) {
	spool := newTestSelectionSpool(t, 68)
	claimsPerRun := int(selectionPlanSortMemoryBytes / selectionClaimBytes)
	writer := bufio.NewWriterSize(spool.claims, int(selectionPlanSortMemoryBytes))
	writeClaim := func(value uint64) {
		t.Helper()
		var raw [selectionClaimBytes]byte
		binary.BigEndian.PutUint64(raw[len(raw)-8:], value)
		if _, err := writer.Write(raw[:]); err != nil {
			t.Fatal(err)
		}
	}
	for index := 1; index <= claimsPerRun; index++ {
		writeClaim(uint64(index))
	}
	writeClaim(uint64(claimsPerRun / 2))
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	spool.claimCount = uint64(claimsPerRun + 1)
	if err := spool.rejectDuplicateClaims(context.Background()); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("cross-run duplicate identity = %v", err)
	}
}

func newTestSelectionSpool(t *testing.T, seed byte) *selectionSpool {
	t.Helper()
	spool, err := newSelectionSpool(transferID[catalog.ShareInstance](seed))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	return spool
}

func TestSelectionPlanRecordCodecAndFrameBoundaries(t *testing.T) {
	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	directoryRecord := selectionPlanRecord{
		kind: selectionPlanDirectoryKind, active: true, path: "directory",
		directory: plannedDirectory{
			directory:  transferID[catalog.DirectoryID](16),
			generation: transferID[catalog.DirectoryGeneration](17),
			path:       "directory",
			modified:   modified,
		},
	}
	fileRecord := selectionPlanRecord{
		kind: selectionPlanFileKind, active: true, path: "directory/file",
		file: plannedFile{
			file:             transferID[catalog.FileID](18),
			path:             "directory/file",
			parentDirectory:  directoryRecord.directory.directory,
			parentGeneration: directoryRecord.directory.generation,
			expectedSize:     42,
		},
	}
	for _, record := range []selectionPlanRecord{directoryRecord, fileRecord} {
		payload, err := encodeSelectionPlanRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeSelectionPlanRecord(payload)
		if err != nil || decoded.kind != record.kind || decoded.active != record.active || decoded.path != record.path {
			t.Fatalf("record round trip = %#v, err %v", decoded, err)
		}
	}

	invalidRecords := []selectionPlanRecord{
		{},
		{kind: 99, active: true, path: "entry"},
		{kind: selectionPlanDirectoryKind, active: true, path: "directory", directory: plannedDirectory{}},
		{kind: selectionPlanDirectoryKind, active: true, path: "directory", directory: plannedDirectory{directory: directoryRecord.directory.directory}},
		{kind: selectionPlanFileKind, active: true, path: "file", file: plannedFile{}},
		{kind: selectionPlanFileKind, active: true, path: "file", file: plannedFile{
			file: transferID[catalog.FileID](19), parentDirectory: directoryRecord.directory.directory,
			parentGeneration: directoryRecord.directory.generation, expectedSize: catalog.MaxFileSize + 1,
		}},
	}
	for index, record := range invalidRecords {
		if _, err := encodeSelectionPlanRecord(record); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("invalid record %d = %v", index, err)
		}
	}

	validDirectory, _ := encodeSelectionPlanRecord(directoryRecord)
	validFile, _ := encodeSelectionPlanRecord(fileRecord)
	invalidActive := slices.Clone(validDirectory)
	invalidActive[1] = 2
	zeroPath := slices.Clone(validDirectory)
	binary.BigEndian.PutUint32(zeroPath[2:6], 0)
	overlongPath := slices.Clone(validDirectory)
	binary.BigEndian.PutUint32(overlongPath[2:6], uint32(len(overlongPath)))
	invalidKind := slices.Clone(validDirectory)
	invalidKind[0] = 99
	shortDirectory := validDirectory[:len(validDirectory)-1]
	inactiveFile := slices.Clone(validFile)
	inactiveFile[1] = 0
	zeroDirectoryIdentity := slices.Clone(validDirectory)
	pathBytes := int(binary.BigEndian.Uint32(zeroDirectoryIdentity[2:6]))
	clear(zeroDirectoryIdentity[6+pathBytes : 6+pathBytes+catalog.IdentityBytes])
	zeroDirectoryGeneration := slices.Clone(validDirectory)
	clear(zeroDirectoryGeneration[6+pathBytes+catalog.IdentityBytes : 6+pathBytes+2*catalog.IdentityBytes])
	filePathBytes := int(binary.BigEndian.Uint32(validFile[2:6]))
	fileIdentityOffset := 6 + filePathBytes
	zeroFileIdentity := slices.Clone(validFile)
	clear(zeroFileIdentity[fileIdentityOffset : fileIdentityOffset+catalog.IdentityBytes])
	zeroFileParent := slices.Clone(validFile)
	clear(zeroFileParent[fileIdentityOffset+catalog.IdentityBytes : fileIdentityOffset+2*catalog.IdentityBytes])
	zeroFileGeneration := slices.Clone(validFile)
	clear(zeroFileGeneration[fileIdentityOffset+2*catalog.IdentityBytes : fileIdentityOffset+3*catalog.IdentityBytes])
	oversizedFile := slices.Clone(validFile)
	binary.BigEndian.PutUint64(oversizedFile[fileIdentityOffset+3*catalog.IdentityBytes:], catalog.MaxFileSize+1)
	invalidModified := slices.Clone(validFile)
	invalidModified[len(invalidModified)-1] = 0xff
	invalidPath := append([]byte{selectionPlanFileKind, 1, 0, 0, 0, 4}, []byte("a//b")...)
	for name, payload := range map[string][]byte{
		"short":                     nil,
		"invalid active":            invalidActive,
		"zero path":                 zeroPath,
		"overlong path":             overlongPath,
		"non-canonical path":        invalidPath,
		"unknown kind":              invalidKind,
		"short directory":           shortDirectory,
		"inactive file":             inactiveFile,
		"zero directory identity":   zeroDirectoryIdentity,
		"zero directory generation": zeroDirectoryGeneration,
		"zero file identity":        zeroFileIdentity,
		"zero file parent":          zeroFileParent,
		"zero file generation":      zeroFileGeneration,
		"oversized file":            oversizedFile,
		"invalid modified time":     invalidModified,
	} {
		t.Run("decode "+name, func(t *testing.T) {
			if _, err := decodeSelectionPlanRecord(payload); err == nil {
				t.Fatal("malformed record was accepted")
			}
		})
	}

	for name, encoded := range map[string][]byte{
		"short":            make([]byte, 13),
		"invalid presence": append([]byte{2}, make([]byte, 13)...),
		"absent with data": append([]byte{0, 1}, make([]byte, 12)...),
	} {
		t.Run("modified "+name, func(t *testing.T) {
			if _, err := decodeSelectionModifiedTime(encoded); !errors.Is(err, ErrSelectionPlanState) {
				t.Fatalf("modified time = %v", err)
			}
		})
	}
	if absent, err := decodeSelectionModifiedTime(make([]byte, 14)); err != nil || absent.Present() {
		t.Fatalf("absent modified time = %#v, err %v", absent, err)
	}

	if err := writeSelectionPlanFrame(io.Discard, nil); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("empty frame = %v", err)
	}
	if err := writeSelectionPlanFrame(io.Discard, make([]byte, maximumSelectionRecordBytes+1)); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("oversized frame = %v", err)
	}
	for failedWrite := 1; failedWrite <= 3; failedWrite++ {
		writer := &selectionFrameFailWriter{failedWrite: failedWrite}
		if err := writeSelectionPlanFrame(writer, validFile); !errors.Is(err, errSelectionFrameWrite) {
			t.Fatalf("write %d failure = %v", failedWrite, err)
		}
	}
	var framed bytes.Buffer
	if err := writeSelectionPlanFrame(&framed, validFile); err != nil {
		t.Fatal(err)
	}
	decoded, err := readSelectionPlanFrame(&framed)
	if err != nil || decoded.path != fileRecord.path {
		t.Fatalf("framed record = %#v, err %v", decoded, err)
	}
	if _, err := readSelectionPlanFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reader = %v", err)
	}
	if _, err := readSelectionPlanFrame(bytes.NewReader(make([]byte, 8))); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("zero-length frame = %v", err)
	}
	truncated := []byte{0, 0, 0, 2, 1}
	if _, err := readSelectionPlanFrame(bytes.NewReader(truncated)); err == nil {
		t.Fatal("truncated frame was accepted")
	}
	badSuffix := append([]byte(nil), framedRecord(t, validFile)...)
	badSuffix[len(badSuffix)-1] ^= 1
	if _, err := readSelectionPlanFrame(bytes.NewReader(badSuffix)); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("mismatched suffix = %v", err)
	}

	path := filepath.Join(t.TempDir(), "terminal.plan")
	plan, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plan.Close() })
	frame := framedRecord(t, validDirectory)
	if _, err := plan.Write(frame); err != nil {
		t.Fatal(err)
	}
	if got, start, err := readSelectionPlanFrameAt(plan, int64(len(frame))); err != nil || start != 0 || got.path != directoryRecord.path {
		t.Fatalf("reverse frame = %#v, start %d, err %v", got, start, err)
	}
	if _, _, err := readSelectionPlanFrameAt(plan, 1); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("short reverse frame = %v", err)
	}
	emptyPlan, err := os.OpenFile(filepath.Join(t.TempDir(), "empty.plan"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emptyPlan.Close() })
	if _, _, err := readSelectionPlanFrameAt(emptyPlan, selectionPlanFrameBytes); err == nil {
		t.Fatal("missing reverse suffix was accepted")
	}
	if _, err := plan.WriteAt([]byte{frame[0] ^ 1}, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSelectionPlanFrameAt(plan, int64(len(frame))); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("mismatched reverse prefix = %v", err)
	}
	if _, err := plan.WriteAt(frame[:1], 0); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.WriteAt([]byte{0xff}, int64(len(frame)-1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSelectionPlanFrameAt(plan, int64(len(frame))); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("invalid reverse length = %v", err)
	}
	malformedPayload := []byte{1}
	malformedFrame := framedRecord(t, malformedPayload)
	malformedPlan, err := os.OpenFile(filepath.Join(t.TempDir(), "malformed.plan"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = malformedPlan.Close() })
	if _, err := malformedPlan.Write(malformedFrame); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSelectionPlanFrameAt(malformedPlan, int64(len(malformedFrame))); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("malformed reverse payload = %v", err)
	}
}

var errSelectionFrameWrite = errors.New("selection frame write failed")

type selectionFrameFailWriter struct {
	writes      int
	failedWrite int
}

func (writer *selectionFrameFailWriter) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failedWrite {
		return 0, errSelectionFrameWrite
	}
	return len(value), nil
}

func framedRecord(t *testing.T, payload []byte) []byte {
	t.Helper()
	var framed bytes.Buffer
	if err := writeSelectionPlanFrame(&framed, payload); err != nil {
		t.Fatal(err)
	}
	return framed.Bytes()
}

func wideSelectionPath(index int) string {
	segments := make([]string, 127)
	for position := range segments {
		segments[position] = strings.Repeat("a", 255)
	}
	return strings.Join(append(segments, fmt.Sprintf("entry-%06d", index)), "/")
}

func selectionSpoolNodeID(t *testing.T, value int) catalog.NodeID {
	t.Helper()
	raw := make([]byte, catalog.IdentityBytes)
	raw[len(raw)-4] = byte(value >> 24)
	raw[len(raw)-3] = byte(value >> 16)
	raw[len(raw)-2] = byte(value >> 8)
	raw[len(raw)-1] = byte(value)
	id, err := catalog.NodeIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
