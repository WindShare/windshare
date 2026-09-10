//go:build linux

package osfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"golang.org/x/sys/unix"
)

func TestLinuxTmpfsPublicOutputPublishesWithoutRecoveryAuthority(t *testing.T) {
	t.Parallel()
	root := linuxPublicTmpfsRoot(t)
	stale := filepath.Join(root, ".windshare-output")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "foreign"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	var capabilities outputcap.DestinationCapabilities
	var traceMu sync.Mutex
	trace := FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traceMu.Lock()
		defer traceMu.Unlock()
		if event.DestinationCapabilities.Valid() {
			capabilities = event.DestinationCapabilities
		}
	})
	authority, intent, session, rootAdmission := openLinuxPublicLiveOperation(t, root, trace)
	resultRoot := nativeDirectTreeResultRoot(t, root, intent)
	payload := []byte("native live output")
	file := nativeDirectTreeTestFile(t, session, intent, 0x41, "download.bin", uint64(len(payload)), rootAdmission)
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || !durable.Ranges().IsEmpty() {
		t.Fatalf("unexpected file admission=%+v", start)
	}
	if _, err := os.Stat(filepath.Join(resultRoot, "download.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified content acquired a public name: %v", err)
	}
	if names := linuxPublicLiveStageNames(t, root); len(names) != 1 {
		t.Fatalf("active process stage count=%d", len(names))
	}
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := transaction.Checkpoint(context.Background()); err != nil || !checkpoint.Ranges().IsEmpty() {
		t.Fatalf("process output claimed restart ranges: %+v error=%v", checkpoint, err)
	}
	if settlement, err := transaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("live publication=%v error=%v", settlement.Kind(), err)
	}
	if names := linuxPublicLiveStageNames(t, root); len(names) != 0 {
		t.Fatalf("committed file retained process stage: %v", names)
	}

	// Install a collision after admission so the final no-replace primitive,
	// rather than only an earlier existence check, has to preserve its contents.
	collision := nativeDirectTreeTestFile(t, session, intent, 0x43, "collision.bin", 4, rootAdmission)
	collisionStart, err := session.BeginFile(context.Background(), collision)
	if err != nil {
		t.Fatal(err)
	}
	collisionTransaction, _, ok := collisionStart.Transaction()
	if !ok {
		t.Fatalf("collision file did not begin: %+v", collisionStart)
	}
	if err := collisionTransaction.WriteRange(context.Background(), 0, []byte("new!")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultRoot, "collision.bin"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if settlement, err := collisionTransaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("late collision=%v error=%v", settlement.Kind(), err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomePartial); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementPartial {
		t.Fatalf("tree settlement=%v error=%v", settlement.Kind(), err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(resultRoot, "download.bin")); err != nil || string(got) != string(payload) {
		t.Fatalf("published bytes=%q error=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(resultRoot, "collision.bin")); err != nil || string(got) != "keep" {
		t.Fatalf("collision target=%q error=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(stale, "foreign")); err != nil || string(got) != "untouched" {
		t.Fatalf("uncertified stale control changed=%q error=%v", got, err)
	}
	if names := linuxPublicLiveStageNames(t, root); len(names) != 0 {
		t.Fatalf("settlement left live stage directories: %v", names)
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	if !capabilities.SafePublish().Supported() || capabilities.OperationRecovery().Supported() ||
		capabilities.RangeRecovery().Supported() || capabilities.CrashCleanup().Supported() {
		t.Fatalf("trace lost independent capability evidence: %+v", capabilities)
	}
}

func TestLinuxTmpfsPublicOutputPauseCleansCurrentProcessStage(t *testing.T) {
	t.Parallel()
	root := linuxPublicTmpfsRoot(t)
	authority, intent, session, rootAdmission := openLinuxPublicLiveOperation(t, root, nil)
	resultRoot := nativeDirectTreeResultRoot(t, root, intent)
	file := nativeDirectTreeTestFile(t, session, intent, 0x51, "partial.bin", 8, rootAdmission)
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("file did not begin: %+v", start)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("half")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if names := linuxPublicLiveStageNames(t, root); len(names) != 0 {
		t.Fatalf("graceful pause retained owned stages: %v", names)
	}
	if _, err := os.Stat(filepath.Join(resultRoot, "partial.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("paused partial was published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".windshare-output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("process-only operation created persistent control state: %v", err)
	}
}

func TestLinuxTmpfsPublicOutputDoesNotAdoptPreviousProcessStage(t *testing.T) {
	const childRootEnvironment = "WINDSHARE_TMPFS_PROCESS_STAGE_ROOT"
	if root := os.Getenv(childRootEnvironment); root != "" {
		_, intent, session, admission := openLinuxPublicLiveOperation(t, root, nil)
		file := nativeDirectTreeTestFile(t, session, intent, 0x61, "interrupted.bin", 8, admission)
		start, err := session.BeginFile(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatal("child did not acquire a process stage")
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte("kept")); err != nil {
			t.Fatal(err)
		}
		// The process exits with live output handles. OS handle closure must not
		// become permission for a later process to delete the remaining names.
		os.Exit(0)
	}
	t.Parallel()
	root := linuxPublicTmpfsRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), childRootEnvironment+"="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("process-stage child failed: %v\n%s", err, output)
	}
	before := linuxPublicLiveStageNames(t, root)
	if len(before) != 1 {
		t.Fatalf("abrupt exit stage count=%d", len(before))
	}
	path := filepath.Join(root, before[0], "partial")
	contentBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if mode, err := reopened.BindDestination(context.Background()); err != nil || !mode.LiveOnly() {
		t.Fatalf("reopened process-only root: mode=%+v error=%v", mode, err)
	}
	if after := linuxPublicLiveStageNames(t, root); !slices.Equal(after, before) {
		t.Fatalf("rebind adopted old stages: before=%v after=%v", before, after)
	}
	if contentAfter, err := os.ReadFile(path); err != nil || string(contentAfter) != string(contentBefore) {
		t.Fatalf("rebind changed abandoned bytes: before=%q after=%q error=%v", contentBefore, contentAfter, err)
	}
}

func linuxPublicTmpfsRoot(t *testing.T) string {
	t.Helper()
	const tmpfsRoot = "/dev/shm"
	var filesystem unix.Statfs_t
	if err := unix.Statfs(tmpfsRoot, &filesystem); err != nil || filesystem.Type != unix.TMPFS_MAGIC {
		t.Skipf("native tmpfs fixture unavailable: %v", err)
	}
	root, err := os.MkdirTemp(tmpfsRoot, "windshare-public-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	return root
}

func linuxPublicLiveStageNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".windshare-live-") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

func openLinuxPublicLiveOperation(
	t *testing.T, root string, tracer FilesystemOutputTracer,
) (*FilesystemOutputAuthority, transfer.ReceiveIntent, transfer.DirectTreeSession, transfer.DirectoryAdmission) {
	t.Helper()
	ctx := context.Background()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root, Tracer: tracer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Close(); err != nil {
			t.Error(err)
		}
	})
	if mode, err := authority.BindDestination(ctx); err != nil || !mode.Valid() || !mode.LiveOnly() || mode.Resumable() {
		t.Fatalf("tmpfs failed public live admission: mode=%+v error=%v", mode, err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](0x31), coverageC6Identity[catalog.DirectoryID](0x32), rules)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil || lookup.Kind() != FilesystemOutputLookupMiss {
		t.Fatalf("lookup=%+v error=%v", lookup, err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !operation.ExecutionMode().LiveOnly() {
		t.Fatal("operation lost live-only mode")
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("operation omitted receive intent")
	}
	session, err := authority.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	if session.Capabilities().Durability != transfer.DurabilityNone {
		t.Fatal("live session claimed durable output")
	}
	request, err := transfer.NewDirectoryMaterializationRequest(intent, transfer.AuthenticatedSourceDirectory{
		DirectoryID: intent.SyntheticRoot(), Generation: coverageC6Identity[catalog.DirectoryGeneration](0x33),
		SourcePath: ordinaryoutput.EmptySourceCatalogPath(),
	}, ordinaryoutput.SourceNodeSelected, transfer.MaterializedDirectoryClaim{})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return authority, intent, session, admission
}
