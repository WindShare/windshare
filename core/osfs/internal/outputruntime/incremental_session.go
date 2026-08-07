package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/incrementaladmission"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// incrementalOutputSession is the root-scoped authority exposed while catalog
// discovery is still running. FileCheckpointV1 is its only durable authority;
// the inner Session reuses native file operations with an in-memory authority
// image and never opens or persists a legacy session header.
//
// The platform is retained from OpenOutput until the first root admission. This
// is intentional: reopening the configured path for the lazy session would turn
// a certified capability into a pathname-only time-of-check/time-of-use gap.
type incrementalOutputSession struct {
	mu           sync.Mutex
	authority    *Authority
	intent       transfer.TransferIntent
	backend      transfer.OutputBackendID
	sessionID    transfer.OutputSessionID
	capabilities transfer.OutputCapabilities

	platform    outputcap.Platform
	rootBinding resumestate.OutputRootBinding
	secret      [sha256.Size]byte
	checkpoint  checkpointstore.Claim
	inner       *Session

	rootGeneration catalog.DirectoryGeneration
	rootOpened     bool
	closed         bool

	// A path has one active catalog generation in a session. Retrying the exact
	// tuple is idempotent; replacing a tuple in place would make an older parent
	// token appear valid for a newer generation, so it is rejected explicitly.
	directories map[string]incrementalDirectoryRecord
	byID        map[catalog.DirectoryID]string
	files       map[string]incrementalFileAdmission
}

var _ transfer.OutputSession = (*incrementalOutputSession)(nil)

type incrementalDirectoryRecord struct {
	directory transfer.OutputDirectory
	admission transfer.DirectoryAdmission
	finalized bool
}

type incrementalFileAdmission struct {
	selection resumestate.LiveFileSelection
	key       resumestate.LiveFileKey
}

func (session *incrementalOutputSession) BackendID() transfer.OutputBackendID {
	if session == nil {
		return ""
	}
	return session.backend
}

func (session *incrementalOutputSession) SessionID() transfer.OutputSessionID {
	if session == nil {
		return transfer.OutputSessionID{}
	}
	return session.sessionID
}

func (session *incrementalOutputSession) Capabilities() transfer.OutputCapabilities {
	if session == nil {
		return transfer.OutputCapabilities{}
	}
	return session.capabilities
}

func (session *incrementalOutputSession) AdmitDirectory(
	ctx context.Context,
	directory transfer.OutputDirectory,
) (transfer.DirectoryAdmission, error) {
	if session == nil || session.authority == nil || ctx == nil {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectoryAdmission{}, err
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return transfer.DirectoryAdmission{}, transfer.ErrOutputSessionFatal
	}
	if err := incrementaladmission.ValidateDirectory(session.intent, directory); err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	session.ensureDirectoryIndexes()
	if admission, handled, err := session.retryDirectoryAdmission(directory); handled {
		return admission, err
	}
	if err := session.validateNewDirectoryAuthority(directory); err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	platform, err := session.validateDirectoryPlatform(directory)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	if err := session.bindRootGeneration(directory); err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	selection, snapshot, validation, err := session.prepareIncrementalSelection(platform, directory)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	if err := session.activatePreparedDirectory(directory, selection, snapshot, validation); err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	admission, err := transfer.NewDirectoryAdmissionWithSecret(session.secret[:], directory)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	session.directories[directory.Path] = incrementalDirectoryRecord{
		directory: directory,
		admission: admission,
	}
	session.byID[directory.DirectoryID] = directory.Path
	session.rootOpened = true

	if err := session.inner.installIncrementalAdmission(
		session.intent.Digest(), selection, snapshot, directory, admission,
	); err != nil {
		delete(session.directories, directory.Path)
		delete(session.byID, directory.DirectoryID)
		return transfer.DirectoryAdmission{}, err
	}
	return admission, nil
}

func (session *incrementalOutputSession) ensureDirectoryIndexes() {
	if session.directories == nil {
		session.directories = make(map[string]incrementalDirectoryRecord)
	}
	if session.byID == nil {
		session.byID = make(map[catalog.DirectoryID]string)
	}
}

func (session *incrementalOutputSession) retryDirectoryAdmission(
	directory transfer.OutputDirectory,
) (transfer.DirectoryAdmission, bool, error) {
	existing, found := session.directories[directory.Path]
	if !found {
		return transfer.DirectoryAdmission{}, false, nil
	}
	if !incrementaladmission.SameDirectory(existing.directory, directory) ||
		!existing.directory.ParentAdmission.Equal(directory.ParentAdmission) {
		return transfer.DirectoryAdmission{}, true, transfer.ErrDirectoryAdmissionMismatch
	}
	return existing.admission, true, nil
}

func (session *incrementalOutputSession) validateNewDirectoryAuthority(
	directory transfer.OutputDirectory,
) error {
	if previousPath, found := session.byID[directory.DirectoryID]; found && previousPath != directory.Path {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	if directory.Path == "" {
		return nil
	}
	parentPath := outputLocatorParentPath(directory.Path)
	parent, found := session.directories[parentPath]
	if !found || parent.finalized || !parent.admission.Equal(directory.ParentAdmission) {
		// A locator never grants ancestry authority. The parent generation must
		// already be admitted before the native walker may materialize a child.
		return transfer.ErrDirectoryAdmissionMismatch
	}
	return nil
}

func (session *incrementalOutputSession) validateDirectoryPlatform(
	directory transfer.OutputDirectory,
) (outputcap.Platform, error) {
	platform := session.platform
	if session.inner != nil {
		platform = session.inner.platform
	}
	if platform == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := platform.ValidateModifiedTime(directory.ModifiedTime); err != nil {
		return nil, err
	}
	if directory.Path != "" {
		if _, err := platform.CanonicalLocatorKey(directory.Path); err != nil {
			return nil, err
		}
	}
	return platform, nil
}

func (session *incrementalOutputSession) bindRootGeneration(
	directory transfer.OutputDirectory,
) error {
	if directory.Path == "" {
		if session.rootOpened {
			return transfer.ErrDirectoryAdmissionMismatch
		}
		session.rootGeneration = directory.Generation
		return nil
	}
	if session.rootGeneration.IsZero() {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	return nil
}

func (session *incrementalOutputSession) activatePreparedDirectory(
	directory transfer.OutputDirectory,
	selection transfer.OutputSelection,
	snapshot outputAncestrySnapshot,
	validation *outputAncestryValidation,
) error {
	if session.inner != nil {
		if closeErr := validation.Close(); closeErr != nil {
			return outputAncestryOperationFault("close incremental ancestry admission", closeErr)
		}
		return nil
	}
	if directory.Path != "" {
		return errors.Join(transfer.ErrDirectoryAdmissionMismatch, validation.Close())
	}
	// The root admission guard stays live through durable namespace creation, so
	// the checkpoint claim is tied to the same certified capability as OpenOutput.
	return session.openInner(selection, snapshot, validation)
}
func (session *incrementalOutputSession) FinalizeDirectory(
	ctx context.Context,
	directory transfer.OutputDirectory,
) error {
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return transfer.ErrOutputSessionFatal
	}
	record, ok := session.directories[directory.Path]
	inner := session.inner
	if !ok || !incrementaladmission.SameDirectory(record.directory, directory) ||
		!record.directory.ParentAdmission.Equal(directory.ParentAdmission) {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	if session.innerHasActiveFilesUnderLocked(directory.Path) {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract,
			fmt.Errorf("cannot finalize directory %q while admitted files are active", directory.Path))
	}
	if inner == nil {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	if err := inner.FinalizeDirectory(ctx, directory); err != nil {
		return err
	}
	if current, exists := session.directories[directory.Path]; exists {
		current.finalized = true
		session.directories[directory.Path] = current
	}
	return nil
}

func (session *incrementalOutputSession) BeginFile(
	ctx context.Context,
	file transfer.OutputFile,
) (transfer.FileStart, error) {
	if session == nil {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	if ctx == nil {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	canonical, canonicalErr := catalog.CanonicalPath(file.Path)
	if canonicalErr != nil || canonical != file.Path {
		return transfer.FileStart{}, transfer.ErrInvalidOutputSelection
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrOutputSessionFatal
	}
	inner := session.inner
	if inner == nil {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrDirectoryAdmissionMismatch
	}
	parentPath := outputLocatorParentPath(file.Path)
	parent, admitted := session.directories[parentPath]
	if !admitted || parent.finalized || !parent.admission.Equal(file.ParentAdmission) {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrDirectoryAdmissionMismatch
	}
	if file.ExpectedSize != file.Descriptor.ExactSize() ||
		file.Descriptor.ShareInstance() != session.intent.ShareInstance() ||
		file.Descriptor.FileID().IsZero() || file.Descriptor.FileRevision().IsZero() {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrInvalidOutputSelection
	}
	target, targetErr := outputTargetForDescriptor(inner.SessionID(), file.Descriptor, file.Path)
	if targetErr != nil || target != file.Target {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrInvalidOutputSelection
	}
	live := resumestate.LiveFileSelection{
		IntentDigest: session.intent.Digest(),
		Selection: transfer.OutputSelectionFile{
			Path: file.Path, FileID: file.Descriptor.FileID(),
			ParentDirectoryID: parent.directory.DirectoryID,
			ParentGeneration:  parent.directory.Generation,
			ExpectedSize:      file.Descriptor.ExactSize(), ModifiedTime: file.Descriptor.ModifiedTime(),
		},
		Revision:        file.Descriptor.FileRevision(),
		ParentAdmission: file.ParentAdmission,
	}
	key, keyErr := live.Key()
	if keyErr != nil {
		session.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrInvalidOutputSelection
	}
	if session.files == nil {
		session.files = make(map[string]incrementalFileAdmission)
	}
	if existing, exists := session.files[file.Path]; exists {
		if existing.key != key || existing.selection.ParentAdmission != live.ParentAdmission ||
			existing.selection.Revision != live.Revision {
			session.mu.Unlock()
			return transfer.FileStart{}, transfer.ErrDirectoryAdmissionMismatch
		}
	} else {
		// The overlay is installed only after the parent receipt, descriptor, target,
		// and immutable key have all been checked. It is never synthesized from a
		// path by the inner execution session.
		if err := inner.installIncrementalFileSelection(live); err != nil {
			session.mu.Unlock()
			return transfer.FileStart{}, err
		}
		session.files[file.Path] = incrementalFileAdmission{selection: live, key: key}
	}
	start, err := inner.BeginFile(ctx, file)
	session.mu.Unlock()
	return start, err
}

// innerHasActiveFilesUnderLocked shares the wrapper admission mutex with
// BeginFile and FinalizeDirectory. The inner active-set check is therefore part
// of the same transition cut as setting final directory metadata.
func (session *incrementalOutputSession) innerHasActiveFilesUnderLocked(path string) bool {
	inner := session.inner
	if inner == nil {
		return false
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	for filePath := range session.files {
		if path != "" && !strings.HasPrefix(filePath, prefix) || path == "" && filePath == "" {
			continue
		}
		if _, active := inner.active[resumestate.DigestCanonicalLocator(filePath)]; active {
			return true
		}
	}
	return false
}

func (session *incrementalOutputSession) PauseJob(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	if session == nil {
		return transfer.JobSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if ctx == nil {
		return transfer.JobSettlement{}, transfer.ErrInvalidOutputBinding
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return transfer.NewJobSettlement(transfer.JobPaused)
	}
	inner := session.inner
	retained := session.platform
	checkpoint := &session.checkpoint
	session.platform = nil
	session.closed = true
	session.mu.Unlock()
	if inner != nil {
		return inner.PauseJob(ctx, reason)
	}
	if retained != nil {
		settlement, settlementErr := transfer.NewJobSettlement(transfer.JobPaused)
		return settlement, errors.Join(settlementErr, checkpoint.Close(), retained.Close())
	}
	return transfer.NewJobSettlement(transfer.JobPaused)
}

func (session *incrementalOutputSession) CompleteJob(
	ctx context.Context,
	outcome transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	if session == nil {
		return transfer.JobSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if ctx == nil || outcome != transfer.JobSucceeded && outcome != transfer.JobCompletedWithErrors {
		return transfer.JobSettlement{}, transfer.ErrInvalidOutputSettlement
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return transfer.NewJobSettlement(transfer.JobClosed)
	}
	inner := session.inner
	retained := session.platform
	checkpoint := &session.checkpoint
	session.platform = nil
	session.closed = true
	session.mu.Unlock()
	if inner != nil {
		return inner.completeIncrementalJob(ctx, outcome)
	}
	if retained != nil {
		settlement, settlementErr := transfer.NewJobSettlement(transfer.JobClosed)
		return settlement, errors.Join(settlementErr, checkpoint.Close(), retained.Close())
	}
	return transfer.NewJobSettlement(transfer.JobClosed)
}

func (session *Session) completeIncrementalJob(
	ctx context.Context,
	outcome transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	if session == nil ||
		(outcome != transfer.JobSucceeded && outcome != transfer.JobCompletedWithErrors) {
		return transfer.JobSettlement{}, transfer.ErrInvalidOutputSettlement
	}
	if err := session.beginSettlement(); err != nil {
		return transfer.JobSettlement{}, err
	}
	defer session.endSettlement()
	if err := ctx.Err(); err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(err)
	}
	session.beginWG.Wait()
	session.mu.Lock()
	active := len(session.active)
	attention := len(session.attention) != 0
	session.mu.Unlock()
	if active != 0 {
		return transfer.JobSettlement{}, session.failOwnerSettlement(outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract,
			fmt.Errorf("%w: %d file transactions remain active", transfer.ErrOutputContract, active),
		))
	}
	validation, err := session.validateOutputAncestry(outputAncestryRequirement{})
	if err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(
			outputAncestryOperationFault("validate ancestry before completing incremental session", err),
		)
	}
	if err := finishOutputAncestryOperation(
		session, validation, outputAncestryRequirement{}, FilesystemOutputAncestrySessionFinalize,
		resumestate.LocatorDigest{}, "finish incremental session ancestry", nil,
	); err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(err)
	}
	if err := session.shutdownOwnerLocked(); err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	settlementKind := transfer.JobClosed
	if attention {
		settlementKind = transfer.JobPausedNeedsAttention
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, err
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, IntentDigest: session.intentDigest,
		SessionID: session.SessionID(), JobSettlement: settlementKind,
	})
	return settlement, nil
}
