//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	windowsV3LegacyJournalPrefix       = ".wsresume-output-"
	windowsV3LegacyJournalIdentityHex  = "11111111111111111111111111111111"
	windowsV3LegacyJournalSuffix       = ".journal"
	windowsV3LegacyJournalBody         = "historic-v2-journal"
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
	session := windowsV3OperationOpen(t, authority, selection)
	file := windowsV3OperationGuardFile(t, session, selection)
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
	if err := os.Rename(rootPath, rootMoved); err != nil {
		t.Fatalf("output-root rename remained blocked after operation cleanup: %v", err)
	}
	if err := os.Rename(rootMoved, rootPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(externalPath, externalMoved); err != nil {
		t.Fatalf("external-ancestor rename remained blocked after session cleanup: %v", err)
	}
	if err := os.Rename(externalMoved, externalPath); err != nil {
		t.Fatal(err)
	}
}

func windowsV3OperationGuardSelection(t *testing.T, size uint64) transfer.OutputSelection {
	t.Helper()
	return windowsV3TestFileSelection(t, []string{windowsV3OperationGuardFilePath}, size)
}

func windowsV3OperationOpen(
	t *testing.T,
	authority *FilesystemOutputAuthority,
	selection transfer.OutputSelection,
) transfer.OutputSession {
	t.Helper()
	session, err := authority.OpenSelection(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func windowsV3OperationGuardFile(
	t *testing.T,
	session transfer.OutputSession,
	selection transfer.OutputSelection,
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
		Path: selected.Path, ExpectedSize: selected.ExpectedSize, Descriptor: descriptor, Target: target,
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

func TestWindowsResumeListingRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	windowsV3CreatePausedResumeSession(t, fixture.rootPath)

	gate := newWindowsV3OperationHoldGate(windowsV3ResumePostcheckHoldTarget)
	t.Cleanup(gate.unblock)
	listingAuthority := windowsResumePostcheckAuthority(t, fixture.rootPath, gate)
	type listResult struct {
		inventory *ResumeStateInventory
		err       error
	}
	gate.enabled.Store(true)
	result := make(chan listResult, 1)
	go func() {
		inventory, err := windowsV3DecoratedListResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: fixture.rootPath}, listingAuthority,
		)
		result <- listResult{inventory: inventory, err: err}
	}()
	waitForWindowsResumePostcheck(t, gate, "listing")
	rootMoveErr, externalMoveErr := fixture.attemptPinnedMoves()
	fixture.assertSentinels(t)
	gate.unblock()

	var listed listResult
	select {
	case listed = <-result:
	case <-time.After(windowsV3OperationHoldTimeout):
		t.Fatal("resume listing did not finish after releasing root revalidation")
	}
	if listed.inventory != nil {
		defer windowsV3CloseResumeInventory(t, listed.inventory)
	}
	assertWindowsResumePlacementPinned(t, rootMoveErr, externalMoveErr)
	if listed.err != nil {
		t.Fatal(listed.err)
	}
	summaries := listed.inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("guarded Windows resume listing = %+v, want one session", summaries)
	}

	// The public-operation guards are closed before ListResumeState returns. A
	// successful discard therefore proves the returned inventory owns its pin.
	settlement, err := DiscardResumeState(context.Background(), summaries[0].Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard through independently pinned inventory = (%+v, %v)", settlement, err)
	}
	if err := listed.inventory.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertSentinels(t)
	fixture.exerciseReplacementRoundTrips(t)
	assertWindowsResumeInventoryEmpty(t, fixture.rootPath)
}

func TestWindowsResumeDiscardRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	windowsV3CreatePausedResumeSession(t, fixture.rootPath)
	inventory, err := ListResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: fixture.rootPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windowsV3CloseResumeInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("Windows discard fixture inventory = %+v", summaries)
	}

	gate := newWindowsV3OperationHoldGate(windowsV3ResumePostcheckHoldTarget)
	t.Cleanup(gate.unblock)
	discardAuthority := windowsResumePostcheckAuthority(t, fixture.rootPath, gate)
	type discardResult struct {
		settlement DiscardSettlement
		err        error
	}
	gate.enabled.Store(true)
	result := make(chan discardResult, 1)
	go func() {
		settlement, err := windowsV3DecoratedDiscardResumeState(
			context.Background(), discardAuthority, summaries[0].Reference,
		)
		result <- discardResult{settlement: settlement, err: err}
	}()
	waitForWindowsResumePostcheck(t, gate, "discard")
	rootMoveErr, externalMoveErr := fixture.attemptPinnedMoves()
	fixture.assertSentinels(t)
	gate.unblock()

	var discarded discardResult
	select {
	case discarded = <-result:
	case <-time.After(windowsV3OperationHoldTimeout):
		t.Fatal("resume discard did not finish after releasing root revalidation")
	}
	assertWindowsResumePlacementPinned(t, rootMoveErr, externalMoveErr)
	if discarded.err != nil || discarded.settlement.Kind != Discarded {
		t.Fatalf("guarded Windows resume discard = (%+v, %v)", discarded.settlement, discarded.err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertSentinels(t)
	fixture.exerciseReplacementRoundTrips(t)
	assertWindowsResumeInventoryEmpty(t, fixture.rootPath)
}

func TestWindowsLegacyDiscardRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	inventory, summary, journalPath := windowsV3ListedLegacyJournal(t, fixture.rootPath)
	defer windowsV3CloseResumeInventory(t, inventory)

	gate := newWindowsV3OperationHoldGate(windowsV3ResumePostcheckHoldTarget)
	t.Cleanup(gate.unblock)
	discardAuthority := windowsResumePostcheckAuthority(t, fixture.rootPath, gate)
	type discardResult struct {
		settlement DiscardSettlement
		err        error
	}
	gate.enabled.Store(true)
	result := make(chan discardResult, 1)
	go func() {
		settlement, err := windowsV3DecoratedDiscardResumeState(
			context.Background(), discardAuthority, summary.Reference,
		)
		result <- discardResult{settlement: settlement, err: err}
	}()
	waitForWindowsResumePostcheck(t, gate, "legacy discard")
	rootMoveErr, externalMoveErr := fixture.attemptPinnedMoves()
	fixture.assertSentinels(t)
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		gate.unblock()
		t.Fatalf("legacy journal still visible at post-mutation revalidation cut: %v", err)
	}
	gate.unblock()

	var discarded discardResult
	select {
	case discarded = <-result:
	case <-time.After(windowsV3OperationHoldTimeout):
		t.Fatal("legacy resume discard did not finish after releasing root revalidation")
	}
	assertWindowsResumePlacementPinned(t, rootMoveErr, externalMoveErr)
	if discarded.err != nil || discarded.settlement.Kind != Discarded {
		t.Fatalf("guarded Windows legacy discard = (%+v, %v)", discarded.settlement, discarded.err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertSentinels(t)
	fixture.exerciseReplacementRoundTrips(t)
	assertWindowsResumeInventoryEmpty(t, fixture.rootPath)
}

func windowsV3ListedLegacyJournal(
	t *testing.T,
	rootPath string,
) (*ResumeStateInventory, ResumeStateSummary, string) {
	t.Helper()
	journalName := windowsV3LegacyJournalPrefix + windowsV3LegacyJournalIdentityHex + windowsV3LegacyJournalSuffix
	journalPath := filepath.Join(rootPath, journalName)
	// V2 journals are intentionally opaque to the v3 inventory. A historical
	// regular-file image is enough to exercise pin-bound legacy removal without
	// claiming the modern runtime can validate retired journal semantics.
	if err := os.WriteFile(journalPath, []byte(windowsV3LegacyJournalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{RootPath: rootPath})
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Reference.Kind() != ResumeStateLegacyUntrusted ||
		windowsV3LegacyHasAttention(summaries[0], "legacy-v2-journal-unreadable") {
		_ = inventory.Close()
		t.Fatalf("list removable legacy journal = %+v", summaries)
	}
	return inventory, summaries[0], journalPath
}

func windowsV3LegacyHasAttention(summary ResumeStateSummary, expected string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == expected {
			return true
		}
	}
	return false
}

type windowsResumePlacementFixture struct {
	externalPath            string
	externalMovedPath       string
	externalReplacementPath string
	rootPath                string
	rootMovedPath           string
	rootReplacementPath     string
	sentinels               map[string]string
}

func newWindowsResumePlacementFixture(t *testing.T) *windowsResumePlacementFixture {
	t.Helper()
	base := t.TempDir()
	fixture := &windowsResumePlacementFixture{
		externalPath:            filepath.Join(base, "external"),
		externalMovedPath:       filepath.Join(base, "external-displaced"),
		externalReplacementPath: filepath.Join(base, "external-replacement"),
	}
	fixture.rootPath = filepath.Join(fixture.externalPath, "output")
	fixture.rootMovedPath = filepath.Join(fixture.externalPath, "output-displaced")
	fixture.rootReplacementPath = filepath.Join(fixture.externalPath, "output-replacement")
	for _, path := range []string{
		fixture.rootPath,
		fixture.rootReplacementPath,
		filepath.Join(fixture.externalReplacementPath, "output"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.sentinels = map[string]string{
		filepath.Join(fixture.rootPath, "receiver-owned.sentinel"):                      "receiver root",
		filepath.Join(fixture.externalPath, "external-owner.sentinel"):                  "receiver ancestry",
		filepath.Join(fixture.rootReplacementPath, "replacement-root.sentinel"):         "replacement root",
		filepath.Join(fixture.externalReplacementPath, "replacement-external.sentinel"): "replacement ancestry",
	}
	for path, content := range fixture.sentinels {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func windowsResumePostcheckAuthority(
	t *testing.T,
	rootPath string,
	gate *windowsV3OperationHoldGate,
) *FilesystemOutputAuthority {
	t.Helper()
	return newOutputV3DecoratedPublicAuthority(t, rootPath, func(platform outputcap.Platform) outputcap.Platform {
		return &windowsV3ResumePostcheckPlatform{
			Platform: platform,
			gate:     gate,
		}
	})
}

func windowsV3CreatePausedResumeSession(t *testing.T, rootPath string) {
	t.Helper()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: rootPath})
	if err != nil {
		t.Fatal(err)
	}
	session := windowsV3OperationOpen(t, authority, publicValuesSelection(t))
	windowsV3OperationCloseSession(t, session)
}

// The facade deliberately owns construction of ordinary listing authorities.
// This bridge exists only in this test file so the already-open outputcap
// decorator can hold the native final-revalidation cut; it projects the same
// opaque public inventory returned by ListResumeState.
func windowsV3DecoratedListResumeState(
	ctx context.Context,
	root FilesystemResumeRoot,
	authority *FilesystemOutputAuthority,
) (*ResumeStateInventory, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	inventory, err := authority.authority.ListResumeState(ctx, root.RootPath)
	if err != nil {
		return nil, err
	}
	return &ResumeStateInventory{authority: inventory}, nil
}

// This is the matching in-flight discard seam. It accepts and returns only the
// facade's opaque reference and projected settlement, never a runtime value.
func windowsV3DecoratedDiscardResumeState(
	ctx context.Context,
	authority *FilesystemOutputAuthority,
	reference ResumeStateRef,
) (DiscardSettlement, error) {
	if authority == nil || authority.authority == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	settlement, err := authority.authority.DiscardResumeState(ctx, reference.authority)
	if err != nil {
		return DiscardSettlement{}, err
	}
	return projectDiscardSettlement(settlement), nil
}

func windowsV3CloseResumeInventory(t *testing.T, inventory *ResumeStateInventory) {
	t.Helper()
	if err := inventory.Close(); err != nil {
		t.Error(err)
	}
}

func waitForWindowsResumePostcheck(
	t *testing.T,
	gate *windowsV3OperationHoldGate,
	operation string,
) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(windowsV3OperationHoldTimeout):
		gate.unblock()
		t.Fatalf("%s did not reach final root revalidation", operation)
	}
}

func (fixture *windowsResumePlacementFixture) attemptPinnedMoves() (error, error) {
	return windowsResumeAttemptPinnedMove(fixture.rootPath, fixture.rootMovedPath),
		windowsResumeAttemptPinnedMove(fixture.externalPath, fixture.externalMovedPath)
}

func windowsResumeAttemptPinnedMove(sourcePath, movedPath string) error {
	err := os.Rename(sourcePath, movedPath)
	if err != nil {
		return err
	}
	restoreErr := os.Rename(movedPath, sourcePath)
	return errors.Join(errors.New("placement move unexpectedly succeeded"), restoreErr)
}

func assertWindowsResumePlacementPinned(t *testing.T, rootMoveErr, externalMoveErr error) {
	t.Helper()
	if !windowsV3OperationGuardBlocksAncestorReplacement(rootMoveErr) {
		t.Fatalf("output-root displacement during final resume revalidation = %v, want placement denial", rootMoveErr)
	}
	if !windowsV3OperationGuardBlocksAncestorReplacement(externalMoveErr) {
		t.Fatalf("external-ancestor displacement during final resume revalidation = %v, want placement denial", externalMoveErr)
	}
}

func windowsV3OperationGuardBlocksAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func (fixture *windowsResumePlacementFixture) assertSentinels(t *testing.T) {
	t.Helper()
	for path, expected := range fixture.sentinels {
		encoded, err := os.ReadFile(path)
		if err != nil || string(encoded) != expected {
			t.Fatalf("resume placement sentinel %q = (%q, %v), want %q", path, encoded, err, expected)
		}
	}
}

func (fixture *windowsResumePlacementFixture) exerciseReplacementRoundTrips(t *testing.T) {
	t.Helper()
	if err := os.Rename(fixture.rootPath, fixture.rootMovedPath); err != nil {
		t.Fatalf("output-root displacement remained blocked after resume cleanup: %v", err)
	}
	if err := os.Rename(fixture.rootReplacementPath, fixture.rootPath); err != nil {
		t.Fatalf("install output-root replacement after cleanup: %v", err)
	}
	fixture.requireSentinel(t, filepath.Join(fixture.rootPath, "replacement-root.sentinel"), "replacement root")
	if err := os.Rename(fixture.rootPath, fixture.rootReplacementPath); err != nil {
		t.Fatalf("remove output-root replacement: %v", err)
	}
	if err := os.Rename(fixture.rootMovedPath, fixture.rootPath); err != nil {
		t.Fatalf("restore original output root: %v", err)
	}

	if err := os.Rename(fixture.externalPath, fixture.externalMovedPath); err != nil {
		t.Fatalf("external-ancestor displacement remained blocked after resume cleanup: %v", err)
	}
	if err := os.Rename(fixture.externalReplacementPath, fixture.externalPath); err != nil {
		t.Fatalf("install external-ancestor replacement after cleanup: %v", err)
	}
	fixture.requireSentinel(t, filepath.Join(fixture.externalPath, "replacement-external.sentinel"), "replacement ancestry")
	if err := os.Rename(fixture.externalPath, fixture.externalReplacementPath); err != nil {
		t.Fatalf("remove external-ancestor replacement: %v", err)
	}
	if err := os.Rename(fixture.externalMovedPath, fixture.externalPath); err != nil {
		t.Fatalf("restore original external ancestry: %v", err)
	}
	fixture.assertSentinels(t)
}

func (fixture *windowsResumePlacementFixture) requireSentinel(t *testing.T, path, expected string) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil || string(encoded) != expected {
		t.Fatalf("replacement sentinel %q = (%q, %v), want %q", path, encoded, err, expected)
	}
}

func assertWindowsResumeInventoryEmpty(
	t *testing.T,
	rootPath string,
) {
	t.Helper()
	inventory, err := ListResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: rootPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windowsV3CloseResumeInventory(t, inventory)
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("restored resume root inventory = %+v, want empty", summaries)
	}
}

func TestWindowsV3DeniedCommitRetainsDeterministicPublishingCut(t *testing.T) {
	rootPath := t.TempDir()
	selection := windowsV3PublishingSelection(t, 4)
	trace := &windowsV3PublishingTrace{}
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: rootPath, Tracer: trace,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := windowsV3OperationOpen(t, authority, selection)
	file := windowsV3OperationGuardFile(t, session, selection)
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
	reopened := windowsV3OperationOpen(t, reopenedAuthority, selection)
	recoveryFile := windowsV3OperationGuardFile(t, reopened, selection)
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
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
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
