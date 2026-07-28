package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3PostAdmissionLinkPermissionLossPausesAndRetries(t *testing.T) {
	t.Parallel()
	const locator = "scoped/file.bin"
	payload := []byte("retained-across-authority-loss")
	root := v3RecoveryRoot(t)
	selection := outputV3PublicationAuthoritySelection(t, locator, uint64(len(payload)))
	sessionIDs := &v3RecoverySessionIDs{}
	gate := &outputV3PublicationPermissionGate{target: locator, failure: syscall.EPERM}
	opened := v3RecoveryOpen(
		t,
		v3RecoveryPublicationPermissionAuthority(t, root, sessionIDs, gate),
		root,
		selection,
	)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || !transfer.RangesCoverFile(uint64(len(payload)), checkpoint.Ranges()) {
		t.Fatalf("complete checkpoint = (ranges=%v, err=%v)", checkpoint.Ranges().Ranges(), err)
	}
	recordBeforePublication := transaction.resumable.Bound().Record()
	sessionID := opened.Session.SessionID()

	settlement, publishErr := transaction.Commit(context.Background())
	if settlement.Kind() != 0 {
		t.Fatalf("failed publication settlement = %v, want no durable settlement", settlement.Kind())
	}
	if !errors.Is(publishErr, syscall.EPERM) {
		t.Fatalf("publication error = %v, want EPERM", publishErr)
	}
	var fault *transfer.OutputFault
	if !errors.As(publishErr, &fault) || fault.Scope() != transfer.OutputFaultFile ||
		fault.Code() != transfer.OutputFaultStateIO {
		t.Fatalf("publication fault = %#v, want file-scoped state-I/O output failure", fault)
	}
	gate.requireCalls(t, 1)
	assertOutputV3PublishingAuthorityRetained(
		t, root, selection, sessionID, recordBeforePublication, uint64(checkpoint.CheckpointGeneration()), payload,
	)

	// A post-admission authority change is a runtime output failure, not evidence
	// that the witnessed object is ambiguous. Pausing preserves the deterministic
	// FilePublishing cut for a fresh process with corrected authority.
	paused, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
	if err != nil || paused.Kind() != transfer.JobPaused {
		t.Fatalf("output-failure pause = (kind=%v, err=%v)", paused.Kind(), err)
	}
	assertOutputV3PublishingAuthorityRetained(
		t, root, selection, sessionID, recordBeforePublication, uint64(checkpoint.CheckpointGeneration()), payload,
	)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
	start, err := reopened.Session.BeginFile(
		context.Background(),
		v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload))),
	)
	published, immediate := start.ImmediateSettlement()
	if err != nil || !immediate || published.Kind() != transfer.FilePublished {
		t.Fatalf("publication after authority recovery = (kind=%v, immediate=%t, err=%v)", published.Kind(), immediate, err)
	}
	finalPath := filepath.Join(root, filepath.FromSlash(locator))
	actual, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("recovered final = %q, %v, want %q", actual, err, payload)
	}
	record := readOutputV3PublicationAuthorityRecord(t, root, selection, sessionID, recordBeforePublication)
	if record.Phase() != resumestate.FilePublished || record.QuarantineReason().Valid() ||
		record.RetirementReason().Valid() || !record.Complete() {
		t.Fatalf(
			"recovered record = (phase=%v, quarantine=%v, retirement=%v, complete=%t)",
			record.Phase(), record.QuarantineReason(), record.RetirementReason(), record.Complete(),
		)
	}
	anchorPath := outputV3PublicationAuthorityAnchorPath(root, selection, sessionID, record)
	if _, err := os.Stat(anchorPath); err != nil {
		t.Fatalf("recovered publication removed its anchor: %v", err)
	}
}

func outputV3PublicationAuthoritySelection(
	t *testing.T,
	locator string,
	exactSize uint64,
) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x31)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x32)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x33)
	parent := v3RecoveryIdentity16[catalog.DirectoryID](0x34)
	parentGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x35)
	modified := v3RecoveryModifiedTime(t)
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: "scoped", DirectoryID: parent, Generation: parentGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path: locator, FileID: v3RecoveryIdentity16[catalog.FileID](0x36),
			ParentDirectoryID: parent, ParentGeneration: parentGeneration,
			ExpectedSize: exactSize, ModifiedTime: modified,
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

func assertOutputV3PublishingAuthorityRetained(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	expected resumestate.FileRecord,
	checkpointGeneration uint64,
	payload []byte,
) {
	t.Helper()
	record := readOutputV3PublicationAuthorityRecord(t, root, selection, sessionID, expected)
	if record.Phase() != resumestate.FilePublishing || record.OutputObject() != expected.OutputObject() ||
		record.CheckpointGeneration() != checkpointGeneration || !record.Complete() ||
		record.QuarantineReason().Valid() || record.RetirementReason().Valid() {
		t.Fatalf(
			"retained record = (phase=%v, object=%v, checkpoint=%d, complete=%t, quarantine=%v, retirement=%v)",
			record.Phase(), record.OutputObject(), record.CheckpointGeneration(), record.Complete(),
			record.QuarantineReason(), record.RetirementReason(),
		)
	}
	stage := resumestate.StageName(record.OutputObject())
	sessionPath := v3RecoverySessionPath(root, selection, sessionID)
	stagePath := filepath.Join(
		sessionPath, resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	anchorPath := outputV3PublicationAuthorityAnchorPath(root, selection, sessionID, record)
	stageInfo, stageErr := os.Stat(stagePath)
	anchorInfo, anchorErr := os.Stat(anchorPath)
	if stageErr != nil || anchorErr != nil || !os.SameFile(stageInfo, anchorInfo) {
		t.Fatalf("retained witness = (stage=%v, anchor=%v, same=%t)", stageErr, anchorErr,
			stageErr == nil && anchorErr == nil && os.SameFile(stageInfo, anchorInfo))
	}
	stageBytes, stageErr := os.ReadFile(stagePath)
	anchorBytes, anchorErr := os.ReadFile(anchorPath)
	if stageErr != nil || anchorErr != nil || !bytes.Equal(stageBytes, payload) || !bytes.Equal(anchorBytes, payload) {
		t.Fatalf("retained witness bytes = (stage=%q/%v, anchor=%q/%v), want %q",
			stageBytes, stageErr, anchorBytes, anchorErr, payload)
	}
	finalPath := filepath.Join(root, filepath.FromSlash(record.CanonicalLocator()))
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("permission failure exposed or changed a final path: %v", err)
	}
}

func readOutputV3PublicationAuthorityRecord(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	expected resumestate.FileRecord,
) resumestate.FileRecord {
	t.Helper()
	name := resumestate.FileRecordName(expected.LocatorDigest())
	path := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.FilesDirectoryName,
		name.Shard(),
		name.Name(),
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read retained publication record: %v", err)
	}
	record, err := resumestate.DecodeFileRecord(raw)
	if err != nil {
		t.Fatalf("decode retained publication record: %v", err)
	}
	return record
}

func outputV3PublicationAuthorityAnchorPath(
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	record resumestate.FileRecord,
) string {
	anchor := resumestate.AnchorName(record.OutputObject())
	return filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID),
		resumestate.AnchorsDirectoryName,
		anchor.Shard(),
		anchor.Name(),
	)
}

type outputV3PublicationPermissionGate struct {
	mu      sync.Mutex
	target  string
	failure error
	calls   int
}

func (gate *outputV3PublicationPermissionGate) reject(path string) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if path != gate.target {
		return nil
	}
	gate.calls++
	return gate.failure
}

func (gate *outputV3PublicationPermissionGate) requireCalls(t *testing.T, want int) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.calls != want {
		t.Fatalf("direct publication link calls = %d, want %d", gate.calls, want)
	}
}

type outputV3PublicationPermissionPlatform struct {
	outputcap.Platform
	gate *outputV3PublicationPermissionGate
}

func (platform *outputV3PublicationPermissionPlatform) Root() outputcap.Directory {
	return &outputV3PublicationPermissionDirectory{
		Directory: platform.Platform.Root(),
		gate:      platform.gate,
	}
}

func (platform *outputV3PublicationPermissionPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return &outputV3PublicationPermissionDirectory{
				Directory: root,
				gate:      platform.gate,
			}
		},
	)
}

type outputV3PublicationPermissionDirectory struct {
	outputcap.Directory
	gate *outputV3PublicationPermissionGate
	path string
}

func (directory *outputV3PublicationPermissionDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationPermissionDirectory{
		Directory: duplicate,
		gate:      directory.gate,
		path:      directory.path,
	}, nil
}

func (directory *outputV3PublicationPermissionDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3PublicationPermissionDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *outputV3PublicationPermissionDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	if private {
		return opened, nil
	}
	path := name
	if directory.path != "" {
		path = directory.path + "/" + name
	}
	return &outputV3PublicationPermissionDirectory{
		Directory: opened,
		gate:      directory.gate,
		path:      path,
	}, nil
}

func (directory *outputV3PublicationPermissionDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil || private {
		return created, err
	}
	path := name
	if directory.path != "" {
		path = directory.path + "/" + name
	}
	return &outputV3PublicationPermissionDirectory{
		Directory: created,
		gate:      directory.gate,
		path:      path,
	}, nil
}

func (directory *outputV3PublicationPermissionDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	path := name
	if directory.path != "" {
		path = directory.path + "/" + name
	}
	if err := directory.gate.reject(path); err != nil {
		return nil, err
	}
	return directory.Directory.LinkFileNoReplace(source, name)
}

func (directory *outputV3PublicationPermissionDirectory) ValidateCreateAuthority() error {
	if validator, ok := directory.Directory.(outputcap.CreateAuthorityValidator); ok {
		return validator.ValidateCreateAuthority()
	}
	return nil
}

func (directory *outputV3PublicationPermissionDirectory) ValidateMetadataAuthority() error {
	if validator, ok := directory.Directory.(outputcap.MetadataAuthorityValidator); ok {
		return validator.ValidateMetadataAuthority()
	}
	return nil
}

func (directory *outputV3PublicationPermissionDirectory) ValidatePublicEntryNames(names []string) error {
	if validator, ok := directory.Directory.(outputcap.PublicEntryNamesValidator); ok {
		return validator.ValidatePublicEntryNames(names)
	}
	for _, name := range names {
		if err := directory.Directory.ValidatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func v3RecoveryPublicationPermissionAuthority(
	t *testing.T,
	root string,
	sessions *v3RecoverySessionIDs,
	gate *outputV3PublicationPermissionGate,
) *Authority {
	t.Helper()
	authority := v3RecoveryAuthority(t, root, sessions)
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &outputV3PublicationPermissionPlatform{Platform: platform, gate: gate}, nil
	}
	return authority
}
