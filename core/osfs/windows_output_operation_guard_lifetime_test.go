//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

const (
	windowsV3OperationHoldTimeout      = 15 * time.Second
	windowsV3ResumePostcheckHoldTarget = "resume-root-revalidation"
	windowsV3OperationGuardFilePath    = "file.bin"
)

type windowsV3OperationHoldGate struct {
	target      string
	enabled     atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newWindowsV3OperationHoldGate(target string) *windowsV3OperationHoldGate {
	return &windowsV3OperationHoldGate{
		target:  target,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (gate *windowsV3OperationHoldGate) hold(name string) {
	if gate == nil || !gate.enabled.Load() || name != gate.target {
		return
	}
	gate.enterOnce.Do(func() { close(gate.entered) })
	<-gate.release
}

func (gate *windowsV3OperationHoldGate) unblock() {
	if gate == nil {
		return
	}
	gate.releaseOnce.Do(func() { close(gate.release) })
}

type windowsV3OperationHoldPlatform struct {
	outputcap.Platform
	gate *windowsV3OperationHoldGate
}

// windowsV3ResumePostcheckPlatform pauses only after the rebound native guard
// has pinned the complete placement chain. This preserves native directory
// identity semantics while exposing the final cut of a resume operation.
type windowsV3ResumePostcheckPlatform struct {
	outputcap.Platform
	gate          *windowsV3OperationHoldGate
	guardAcquires atomic.Uint32
}

func (platform *windowsV3ResumePostcheckPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if platform.guardAcquires.Add(1) == 2 {
		platform.gate.hold(windowsV3ResumePostcheckHoldTarget)
	}
	return guard, nil
}

func (platform *windowsV3OperationHoldPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &windowsV3OperationHoldGuard{
		PublicOperationGuard: guard,
		root: &windowsV3OperationHoldDirectory{
			Directory: guard.Root(),
			gate:      platform.gate,
		},
	}, nil
}

type windowsV3OperationHoldGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *windowsV3OperationHoldGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *windowsV3OperationHoldGuard) Close() error {
	if guard == nil || guard.PublicOperationGuard == nil {
		return nil
	}
	err := guard.PublicOperationGuard.Close()
	guard.PublicOperationGuard = nil
	guard.root = nil
	return err
}

type windowsV3OperationHoldDirectory struct {
	outputcap.Directory
	gate *windowsV3OperationHoldGate
	path string
}

func (directory *windowsV3OperationHoldDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	directory.gate.hold(directory.childPath(name))
	return directory.Directory.ObserveEntry(name)
}

func (directory *windowsV3OperationHoldDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &windowsV3OperationHoldDirectory{
		Directory: duplicate,
		gate:      directory.gate,
		path:      directory.path,
	}, nil
}

func (directory *windowsV3OperationHoldDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*windowsV3OperationHoldDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *windowsV3OperationHoldDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil || private {
		return opened, err
	}
	return directory.wrap(opened, name), nil
}

func (directory *windowsV3OperationHoldDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil || private {
		return created, err
	}
	return directory.wrap(created, name), nil
}

func (directory *windowsV3OperationHoldDirectory) wrap(
	owned outputcap.Directory,
	name string,
) outputcap.Directory {
	return &windowsV3OperationHoldDirectory{
		Directory: owned,
		gate:      directory.gate,
		path:      directory.childPath(name),
	}
}

func (directory *windowsV3OperationHoldDirectory) childPath(name string) string {
	if directory.path == "" {
		return name
	}
	return directory.path + "/" + name
}

func TestWindowsV3RecoveryRetainsFullPlacementGuardThroughFinalObservation(t *testing.T) {
	base := t.TempDir()
	externalPath := filepath.Join(base, "external")
	rootPath := filepath.Join(externalPath, "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}

	gate := newWindowsV3OperationHoldGate(windowsV3OperationGuardFilePath)
	t.Cleanup(gate.unblock)
	authority := newOutputV3DecoratedPublicAuthority(t, rootPath, func(platform outputcap.Platform) outputcap.Platform {
		return &windowsV3OperationHoldPlatform{Platform: platform, gate: gate}
	})
	selection := windowsV3OperationGuardSelection(t, 4)
	session, parentAdmission := windowsV3OperationOpen(t, authority, rootPath, selection)
	file := windowsV3OperationGuardFile(t, session, selection, parentAdmission)
	transaction := windowsV3OperationBeginTransaction(t, session, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	type commitResult struct {
		settlement transfer.FileSettlement
		err        error
	}
	gate.enabled.Store(true)
	result := make(chan commitResult, 1)
	go func() {
		settlement, err := transaction.Commit(context.Background())
		result <- commitResult{settlement: settlement, err: err}
	}()

	select {
	case <-gate.entered:
	case <-time.After(windowsV3OperationHoldTimeout):
		gate.unblock()
		t.Fatal("commit did not reach guarded final-file observation")
	}

	rootMoved := filepath.Join(externalPath, "output-moved")
	rootRenameErr := os.Rename(rootPath, rootMoved)
	if rootRenameErr == nil {
		_ = os.Rename(rootMoved, rootPath)
	}
	externalMoved := filepath.Join(base, "external-moved")
	externalRenameErr := os.Rename(externalPath, externalMoved)
	if externalRenameErr == nil {
		_ = os.Rename(externalMoved, externalPath)
	}

	gate.unblock()
	var committed commitResult
	select {
	case committed = <-result:
	case <-time.After(windowsV3OperationHoldTimeout):
		t.Fatal("commit did not finish after releasing final-file observation")
	}
	if committed.err != nil || committed.settlement.Kind() != transfer.FilePublished {
		t.Fatalf("guarded commit = (kind=%v, err=%v), want published", committed.settlement.Kind(), committed.err)
	}
	if !windowsV3OperationGuardBlocksAncestorReplacement(rootRenameErr) {
		t.Fatalf("output-root rename while final observation held = %v, want placement denial", rootRenameErr)
	}
	if !windowsV3OperationGuardBlocksAncestorReplacement(externalRenameErr) {
		t.Fatalf("external-ancestor rename while final observation held = %v, want placement denial", externalRenameErr)
	}

	windowsV3OperationCloseSession(t, session)
	// Renaming the highest retained ancestor is the strongest cleanup
	// observation: any surviving output-root or ancestry pin would deny it.
	// Keep this one-way because reversing a freshly renamed non-empty tree lets
	// host filesystem filters race the test without adding an authority check.
	if err := os.Rename(externalPath, externalMoved); err != nil {
		t.Fatalf("full-placement rename remained blocked after session cleanup: %v", err)
	}
}

func windowsV3OperationGuardSelection(t *testing.T, size uint64) transfer.OutputSelection {
	t.Helper()
	return windowsV3TestFileSelection(t, []string{windowsV3OperationGuardFilePath}, size)
}

func windowsV3OperationOpen(
	t *testing.T,
	authority *FilesystemOutputAuthority,
	rootPath string,
	selection transfer.OutputSelection,
) (transfer.OutputSession, transfer.DirectoryAdmission) {
	t.Helper()
	session, admissions, err := openOutputSelectionFixture(t, authority, rootPath, selection)
	if err != nil {
		t.Fatal(err)
	}
	parentPath := ""
	if files := selection.Files(); len(files) != 0 {
		parentPath = files[0].Path
		if slash := strings.LastIndexByte(parentPath, '/'); slash >= 0 {
			parentPath = parentPath[:slash]
		} else {
			parentPath = ""
		}
	}
	return session, admissions[parentPath]
}

func windowsV3OperationGuardFile(
	t *testing.T,
	session transfer.OutputSession,
	selection transfer.OutputSelection,
	parentAdmission transfer.DirectoryAdmission,
) transfer.OutputFile {
	t.Helper()
	selected := selection.Files()[0]
	geometry, err := content.NewFileGeometry(selected.ExpectedSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		selection.ShareInstance(), selected.FileID,
		windowsV3TestIdentity16[content.FileRevision](0xa1), geometry, selected.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	locator, err := transfer.NewPathOutputLocator(selected.Path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(session.BackendID(), session.SessionID(), descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Path: selected.Path, ExpectedSize: selected.ExpectedSize, Descriptor: descriptor,
		Target: target, ParentAdmission: parentAdmission,
	}
}

func windowsV3OperationBeginTransaction(
	t *testing.T,
	session transfer.OutputSession,
	file transfer.OutputFile,
) transfer.FileTransaction {
	t.Helper()
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		settlement, _ := start.ImmediateSettlement()
		t.Fatalf("begin guarded output file = immediate %v, want transaction", settlement.Kind())
	}
	return transaction
}

func windowsV3OperationCloseSession(t *testing.T, session transfer.OutputSession) {
	t.Helper()
	settlement, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil || settlement.Kind() != transfer.JobPaused {
		t.Fatalf("close guarded output session = (kind=%v, err=%v)", settlement.Kind(), err)
	}
}

func windowsV3OperationGuardBlocksAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func TestWindowsV3DeniedCommitRetainsDeterministicPublishingCut(t *testing.T) {
	t.Skip("legacy v3 restart publication is retired; FileCheckpointV1 must authorize incremental recovery")
	rootPath := t.TempDir()
	selection := windowsV3PublishingSelection(t, 4)
	trace := &windowsV3PublishingTrace{}
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: rootPath, Tracer: trace,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, parentAdmission := windowsV3OperationOpen(t, authority, rootPath, selection)
	file := windowsV3OperationGuardFile(t, session, selection, parentAdmission)
	transaction := windowsV3OperationBeginTransaction(t, session, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	parentPath := filepath.Join(rootPath, windowsV3PublishingDirectory)
	displacedPath := filepath.Join(filepath.Dir(rootPath), filepath.Base(rootPath)+"-publishing-cut-displaced")
	t.Cleanup(func() {
		_ = os.Rename(displacedPath, parentPath)
		_ = os.RemoveAll(displacedPath)
	})
	if err := os.Rename(parentPath, displacedPath); err != nil {
		t.Fatal(err)
	}
	settlement, commitErr := transaction.Commit(context.Background())
	if settlement.Kind() != 0 || !windowsV3FailureRequiresJobPause(commitErr) {
		t.Fatalf("commit through displaced ancestry = (kind=%v, err=%v), want retained pause", settlement.Kind(), commitErr)
	}
	if !trace.SawPublishingTransition() {
		t.Fatal("denied publication did not emit the durable Publishing transition")
	}
	for label, finalPath := range map[string]string{
		"restored namespace":  filepath.Join(rootPath, filepath.FromSlash(windowsV3PublishingFilePath)),
		"displaced namespace": filepath.Join(displacedPath, "file.bin"),
	} {
		if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s acquired a premature final: %v", label, err)
		}
	}
	paused, err := session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
	if err != nil || paused.Kind() != transfer.JobPaused {
		t.Fatalf("pause retained publication = (kind=%v, err=%v)", paused.Kind(), err)
	}
	if err := os.Rename(displacedPath, parentPath); err != nil {
		t.Fatal(err)
	}

	reopenedAuthority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: rootPath})
	if err != nil {
		t.Fatal(err)
	}
	reopened, reopenedAdmission := windowsV3OperationOpen(t, reopenedAuthority, rootPath, selection)
	recoveryFile := windowsV3OperationGuardFile(t, reopened, selection, reopenedAdmission)
	start, err := reopened.BeginFile(context.Background(), recoveryFile)
	settlement, immediate := start.ImmediateSettlement()
	if err != nil || !immediate || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("restored deterministic cut = (kind=%v, immediate=%t, err=%v), want published", settlement.Kind(), immediate, err)
	}
	windowsV3OperationCloseSession(t, reopened)
}

const (
	windowsV3PublishingDirectory = "guarded"
	windowsV3PublishingFilePath  = windowsV3PublishingDirectory + "/file.bin"
)

func windowsV3PublishingSelection(t *testing.T, size uint64) transfer.OutputSelection {
	t.Helper()
	share := windowsV3TestIdentity16[catalog.ShareInstance](0xc1)
	root := windowsV3TestIdentity16[catalog.DirectoryID](0xc2)
	rootGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](0xc3)
	parent := windowsV3TestIdentity16[catalog.DirectoryID](0xc4)
	parentGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](0xc5)
	modified := windowsV3TestModifiedTime(t)
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: windowsV3PublishingDirectory, DirectoryID: parent,
			Generation: parentGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path:              windowsV3PublishingFilePath,
			FileID:            windowsV3TestIdentity16[catalog.FileID](0xc6),
			ParentDirectoryID: parent, ParentGeneration: parentGeneration,
			ExpectedSize: size, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

type windowsV3PublishingTrace struct {
	publishingTransitions int
}

func (trace *windowsV3PublishingTrace) TraceFilesystemOutput(event FilesystemOutputTrace) {
	if event.Operation == TraceFilePhaseTransition && event.NextPhase == FilesystemOutputFilePublishing {
		trace.publishingTransitions++
	}
}

func (trace *windowsV3PublishingTrace) SawPublishingTransition() bool {
	return trace != nil && trace.publishingTransitions != 0
}

func windowsV3FailureRequiresJobPause(err error) bool {
	sessionErr, found := errors.AsType[*transfer.OutputSessionError](err)
	return found && sessionErr.RequiresJobPause()
}
