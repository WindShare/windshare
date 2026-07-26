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

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	windowsV3OperationHoldTimeout      = 15 * time.Second
	windowsV3ResumePostcheckHoldTarget = "resume-root-revalidation"
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
	outputV3Platform
	gate *windowsV3OperationHoldGate
}

// windowsV3ResumePostcheckPlatform pauses only after the rebound native guard
// has pinned the complete placement chain. This preserves native directory
// identity semantics while exposing the final cut of a resume operation.
type windowsV3ResumePostcheckPlatform struct {
	outputV3Platform
	gate          *windowsV3OperationHoldGate
	guardAcquires atomic.Uint32
}

func (platform *windowsV3ResumePostcheckPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	guard, err := platform.outputV3Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if platform.guardAcquires.Add(1) == 2 {
		platform.gate.hold(windowsV3ResumePostcheckHoldTarget)
	}
	return guard, nil
}

func (platform *windowsV3OperationHoldPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	guard, err := platform.outputV3Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &windowsV3OperationHoldGuard{
		outputV3PublicOperationGuard: guard,
		root: &windowsV3OperationHoldDirectory{
			outputV3Directory: guard.Root(),
			gate:              platform.gate,
		},
	}, nil
}

type windowsV3OperationHoldGuard struct {
	outputV3PublicOperationGuard
	root outputV3Directory
}

func (guard *windowsV3OperationHoldGuard) Root() outputV3Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *windowsV3OperationHoldGuard) Close() error {
	if guard == nil || guard.outputV3PublicOperationGuard == nil {
		return nil
	}
	err := guard.outputV3PublicOperationGuard.Close()
	guard.outputV3PublicOperationGuard = nil
	guard.root = nil
	return err
}

type windowsV3OperationHoldDirectory struct {
	outputV3Directory
	gate *windowsV3OperationHoldGate
	path string
}

func (directory *windowsV3OperationHoldDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	directory.gate.hold(directory.childPath(name))
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *windowsV3OperationHoldDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &windowsV3OperationHoldDirectory{
		outputV3Directory: duplicate,
		gate:              directory.gate,
		path:              directory.path,
	}, nil
}

func (directory *windowsV3OperationHoldDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if wrapped, ok := other.(*windowsV3OperationHoldDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *windowsV3OperationHoldDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil || private {
		return opened, err
	}
	return directory.wrap(opened, name), nil
}

func (directory *windowsV3OperationHoldDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil || private {
		return created, err
	}
	return directory.wrap(created, name), nil
}

func (directory *windowsV3OperationHoldDirectory) wrap(
	owned outputV3Directory,
	name string,
) outputV3Directory {
	return &windowsV3OperationHoldDirectory{
		outputV3Directory: owned,
		gate:              directory.gate,
		path:              directory.childPath(name),
	}
}

func (directory *windowsV3OperationHoldDirectory) childPath(name string) string {
	if directory.path == "" {
		return name
	}
	return directory.path + "/" + name
}

func TestWindowsV3RecoveryRetainsFullPlacementGuardThroughFinalObservation(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	externalPath := filepath.Join(base, "external")
	rootPath := filepath.Join(externalPath, "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}

	gate := newWindowsV3OperationHoldGate(v3RecoveryFilePath)
	t.Cleanup(gate.unblock)
	authority := v3RecoveryAuthority(t, rootPath, nil)
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3OperationHoldPlatform{outputV3Platform: platform, gate: gate}, nil
	}
	selection := v3RecoverySelection(t, true, 4)
	opened := v3RecoveryOpen(t, authority, rootPath, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, 4)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
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
	if !v3RecoveryIsBlockedAncestorReplacement(rootRenameErr) {
		t.Fatalf("output-root rename while final observation held = %v, want placement denial", rootRenameErr)
	}
	if !v3RecoveryIsBlockedAncestorReplacement(externalRenameErr) {
		t.Fatalf("external-ancestor rename while final observation held = %v, want placement denial", externalRenameErr)
	}

	v3RecoveryCloseSession(t, opened.Session)
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

func TestWindowsResumeListingRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	base := v3RecoveryAuthority(t, fixture.rootPath, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, base, fixture.rootPath, selection)
	v3RecoveryCloseSession(t, opened.Session)

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
		inventory, err := listingAuthority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: fixture.rootPath},
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
		defer v3RecoveryCloseInventory(t, listed.inventory)
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
	settlement, err := base.discardResumeState(context.Background(), summaries[0].Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard through independently pinned inventory = (%+v, %v)", settlement, err)
	}
	if err := listed.inventory.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertSentinels(t)
	fixture.exerciseReplacementRoundTrips(t)
	assertWindowsResumeInventoryEmpty(t, base, fixture.rootPath)
}

func TestWindowsResumeDiscardRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	base := v3RecoveryAuthority(t, fixture.rootPath, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, base, fixture.rootPath, selection)
	v3RecoveryCloseSession(t, opened.Session)
	inventory, err := base.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: fixture.rootPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
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
		settlement, err := discardAuthority.discardResumeState(
			context.Background(), summaries[0].Reference,
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
	assertWindowsResumeInventoryEmpty(t, base, fixture.rootPath)
}

func TestWindowsLegacyDiscardRetainsPlacementThroughFinalRevalidation(t *testing.T) {
	fixture := newWindowsResumePlacementFixture(t)
	base, inventory, summary, journalPath, _ := v3RecoveryListedLegacyV2Journal(t, fixture.rootPath)
	defer v3RecoveryCloseInventory(t, inventory)

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
		settlement, err := discardAuthority.discardResumeState(context.Background(), summary.Reference)
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
	assertWindowsResumeInventoryEmpty(t, base, fixture.rootPath)
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
	base := windowsV3NativeTestTempDir(t)
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
	authority := v3RecoveryAuthority(t, rootPath, nil)
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ResumePostcheckPlatform{
			outputV3Platform: platform,
			gate:             gate,
		}, nil
	}
	return authority
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
	if !v3RecoveryIsBlockedAncestorReplacement(rootMoveErr) {
		t.Fatalf("output-root displacement during final resume revalidation = %v, want placement denial", rootMoveErr)
	}
	if !v3RecoveryIsBlockedAncestorReplacement(externalMoveErr) {
		t.Fatalf("external-ancestor displacement during final resume revalidation = %v, want placement denial", externalMoveErr)
	}
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
	authority *FilesystemOutputAuthority,
	rootPath string,
) {
	t.Helper()
	inventory, err := authority.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: rootPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("restored resume root inventory = %+v, want empty", summaries)
	}
}

func TestWindowsV3DeniedCommitRetainsDeterministicPublishingCut(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	selection := windowsV3PlacementSelection(t, 4)
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, rootPath, sessionIDs), rootPath, selection)
	t.Cleanup(func() { _ = opened.Session.closeHandles() })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 4)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	parentPath := filepath.Join(rootPath, windowsV3PlacementDirectory)
	displacedPath := filepath.Join(filepath.Dir(rootPath), filepath.Base(rootPath)+"-publishing-cut-displaced")
	t.Cleanup(func() {
		_ = os.Rename(displacedPath, parentPath)
		_ = os.RemoveAll(displacedPath)
	})
	if err := os.Rename(parentPath, displacedPath); err != nil {
		t.Fatal(err)
	}
	settlement, commitErr := transaction.Commit(context.Background())
	if settlement.Kind() != 0 || !outputV3FailureRequiresJobPause(commitErr) {
		t.Fatalf("commit through displaced ancestry = (kind=%v, err=%v), want retained pause", settlement.Kind(), commitErr)
	}
	persisted := outputV3PersistedFileRecord(t, opened.Session, windowsV3PlacementFile)
	if persisted.Phase() != resumestate.FilePublishing {
		t.Fatalf("denied publication phase = %v, want Publishing", persisted.Phase())
	}
	for label, finalPath := range map[string]string{
		"restored namespace":  filepath.Join(rootPath, filepath.FromSlash(windowsV3PlacementFile)),
		"displaced namespace": filepath.Join(displacedPath, "file.bin"),
	} {
		if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s acquired a premature final: %v", label, err)
		}
	}
	paused, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
	if err != nil || paused.Kind() != transfer.JobPaused {
		t.Fatalf("pause retained publication = (kind=%v, err=%v)", paused.Kind(), err)
	}
	if err := os.Rename(displacedPath, parentPath); err != nil {
		t.Fatal(err)
	}

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, rootPath, sessionIDs), rootPath, selection)
	recoveryFile := v3RecoveryOutputFile(t, reopened.Session, selection, 4)
	start, err := reopened.Session.BeginFile(context.Background(), recoveryFile)
	settlement, immediate := start.ImmediateSettlement()
	if err != nil || !immediate || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("restored deterministic cut = (kind=%v, immediate=%t, err=%v), want published", settlement.Kind(), immediate, err)
	}
	v3RecoveryCloseSession(t, reopened.Session)
}
