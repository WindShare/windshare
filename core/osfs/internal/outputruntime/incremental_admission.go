package outputruntime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *incrementalOutputSession) openInner(
	selection transfer.OutputSelection,
	snapshot outputAncestrySnapshot,
	validation *outputAncestryValidation,
) error {
	if session.platform == nil || session.authority == nil || validation == nil {
		if validation != nil {
			return errors.Join(transfer.ErrInvalidOutputBinding, validation.Close())
		}
		return transfer.ErrInvalidOutputBinding
	}
	retained := session.platform
	admission, err := preflightOutputSelectionAdmissionWithIntent(retained, selection, session.intent.Digest())
	if err != nil {
		return errors.Join(err, validation.Close())
	}
	admission.admissionSecret = session.secret
	admission.ancestry = snapshot
	admission.validation = validation
	admission.incremental = &incrementalOutputAdmission{
		selection:    selection,
		intentDigest: session.intent.Digest(),
		rootBinding:  session.rootBinding,
	}
	if err := session.authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return errors.Join(err, session.authority.closeOutputAdmissionAncestry(&admission))
	}
	runtimeBinding, err := resumestate.NewCheckpointRuntimeBinding(
		session.sessionID, session.intent.Digest(), filesystemOutputBackendID, session.rootBinding.Bytes(),
	)
	if err != nil {
		return errors.Join(err, session.authority.closeOutputAdmissionAncestry(&admission))
	}
	claim := &session.checkpoint
	if claim.Intent == nil || claim.Records == nil || claim.Anchors == nil ||
		claim.Stages == nil || claim.Lock == nil {
		return errors.Join(transfer.ErrInvalidOutputBinding, session.authority.closeOutputAdmissionAncestry(&admission))
	}
	inner := &Session{
		owner: session.authority, platform: retained, sessionDir: claim.Intent,
		anchorsDir: claim.Anchors, stagesDir: claim.Stages,
		checkpointsDir: claim.Records, sessionLock: claim.Lock, checkpointRuntime: runtimeBinding,
		sessionID: session.sessionID, selection: selection, intentDigest: session.intent.Digest(),
		ancestry:     snapshot,
		capabilities: session.capabilities, selectedFiles: admission.files, selectedDirs: admission.dirs,
		admittedDirs: make(map[string]transfer.DirectoryAdmission), admissionSecret: session.secret,
		objectClaims: make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
		beginning:    make(map[resumestate.LocatorDigest]struct{}), active: make(map[resumestate.LocatorDigest]*FileTransaction),
	}
	if err := inner.restoreIncrementalAdmission(admission.incremental); err != nil {
		return errors.Join(err, session.authority.closeOutputAdmissionAncestry(&admission))
	}
	attention, err := inner.loadPersistedIncrementalCheckpoints()
	if err != nil {
		return errors.Join(err, session.authority.closeOutputAdmissionAncestry(&admission))
	}
	inner.attention = attention
	inner.exposed = true
	if err := session.authority.closeOutputAdmissionAncestry(&admission); err != nil {
		return err
	}
	// Transfer every capability as one ownership cut. No legacy session directory
	// or header was opened, so a later catalog root generation cannot veto these
	// intent-bound checkpoints.
	*claim = checkpointstore.Claim{}
	session.platform = nil
	session.inner = inner
	session.authority.trace(FilesystemOutputTrace{
		Operation: TraceSessionOpened, IntentDigest: session.intent.Digest(), SessionID: session.sessionID,
	})
	return nil
}

func (session *incrementalOutputSession) prepareIncrementalSelection(
	platform outputcap.Platform,
	candidate transfer.OutputDirectory,
) (transfer.OutputSelection, outputAncestrySnapshot, *outputAncestryValidation, error) {
	if platform == nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, transfer.ErrInvalidOutputBinding
	}
	selection, err := session.incrementalSelection(candidate)
	if err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, err
	}
	if err := session.validateIncrementalSelection(platform, selection); err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, err
	}
	previousSnapshot := session.incrementalAncestrySnapshot()
	if err := session.revalidateIncrementalDirectories(platform); err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, err
	}
	if err := validateIncrementalCandidateParent(platform, candidate.Path); err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, err
	}

	// The preparation callback is the only place where the candidate can be
	// created. It runs while the native placement guard is held.
	validation, err := captureOutputSelectionAncestryWithGuardedPreparation(
		platform, selection, outputAncestryRequirement{},
		func(root outputcap.Directory) error { return materializeOutputSelection(root, selection) },
	)
	if err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, err
	}
	if err := validateRetainedIncrementalAncestry(previousSnapshot, validation.snapshot); err != nil {
		return transfer.OutputSelection{}, outputAncestrySnapshot{}, nil, errors.Join(err, validation.Close())
	}
	return selection, validation.snapshot, validation, nil
}

func (session *incrementalOutputSession) incrementalSelection(
	candidate transfer.OutputDirectory,
) (transfer.OutputSelection, error) {
	directories := make([]transfer.OutputSelectionDirectory, 0, len(session.directories)+1)
	for _, record := range session.directories {
		// The synthetic root is carried by OutputSelection.root/rootGeneration;
		// inserting it into the directory claims would violate the canonical
		// selection path contract (empty paths are never selection records).
		if record.directory.Path == "" {
			continue
		}
		directories = append(directories, transfer.OutputSelectionDirectory{
			Path: record.directory.Path, DirectoryID: record.directory.DirectoryID,
			Generation: record.directory.Generation, ModifiedTime: record.directory.ModifiedTime,
		})
	}
	if _, exists := session.directories[candidate.Path]; !exists && candidate.Path != "" {
		directories = append(directories, transfer.OutputSelectionDirectory{
			Path: candidate.Path, DirectoryID: candidate.DirectoryID,
			Generation: candidate.Generation, ModifiedTime: candidate.ModifiedTime,
		})
	}
	selection, err := transfer.NewOutputSelection(
		session.intent.ShareInstance(), session.intent.SyntheticRoot(), session.rootGeneration,
		directories, nil,
	)
	if err != nil {
		return transfer.OutputSelection{}, err
	}
	request, err := transfer.NewCanonicalSelectionRequest(
		session.intent.ShareInstance(), session.intent.SyntheticRoot(), session.intent.SelectionRules(),
	)
	if err != nil {
		return transfer.OutputSelection{}, err
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, selection)
	if err != nil {
		return transfer.OutputSelection{}, err
	}
	return canonical.BindPlan(selection)
}

func (session *incrementalOutputSession) validateIncrementalSelection(
	platform outputcap.Platform,
	selection transfer.OutputSelection,
) error {
	if err := validateReservedOutputSelection(platform, selection); err != nil {
		return err
	}
	if _, err := preflightOutputSelectionAdmissionWithIntent(platform, selection, session.intent.Digest()); err != nil {
		return err
	}
	if err := platform.ValidateSelectionMetadata(selection); err != nil {
		return err
	}
	if err := preflightOutputSelectionAuthorities(platform, selection); err != nil {
		return err
	}
	return platform.ProbeRecoverableFeatures()
}

func (session *incrementalOutputSession) incrementalAncestrySnapshot() outputAncestrySnapshot {
	if session.inner == nil {
		return outputAncestrySnapshot{}
	}
	session.inner.mu.Lock()
	defer session.inner.mu.Unlock()
	return session.inner.ancestry
}

func (session *incrementalOutputSession) revalidateIncrementalDirectories(platform outputcap.Platform) error {
	// Existing admitted paths must remain present and unchanged. Only the final
	// candidate may be absent; its parent is checked read-only before preparation.
	for path := range session.directories {
		if path == "" {
			continue
		}
		opened, err := openOutputDirectoryPath(platform.Root(), path, false)
		if err != nil {
			return err
		}
		if err := opened.Close(); err != nil {
			return err
		}
	}
	return nil
}

func validateIncrementalCandidateParent(platform outputcap.Platform, candidatePath string) error {
	if candidatePath == "" {
		return nil
	}
	parentPath := outputLocatorParentPath(candidatePath)
	if parentPath == "" {
		return nil
	}
	parent, err := openOutputDirectoryPath(platform.Root(), parentPath, false)
	if err != nil {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	return parent.Close()
}

func validateRetainedIncrementalAncestry(previousSnapshot, snapshot outputAncestrySnapshot) error {
	for _, previous := range previousSnapshot.entries {
		current, found := snapshot.claim(previous.path)
		if !found || !current.Equal(previous.claim) {
			return errors.Join(
				errOutputAncestryUnsafe, errOutputAncestryMismatch,
				fmt.Errorf("previously admitted output path %q changed", previous.path),
			)
		}
	}
	return nil
}

// incrementalOutputAdmission is the live authority variant carried across an
// in-process terminal reopen. It authorizes dynamic ancestry while each file's
// durable identity and progress remain exclusively in FileCheckpointV1.
type incrementalOutputAdmission struct {
	selection        transfer.OutputSelection
	intentDigest     transfer.TransferIntentDigest
	rootBinding      resumestate.OutputRootBinding
	admittedDirs     map[string]transfer.DirectoryAdmission
	files            map[string]resumestate.LiveFileSelection
	checkpoints      map[resumestate.LiveFileKey]resumestate.FileCheckpointV1
	checkpointByPath map[string]resumestate.FileCheckpointV1
}

func (session *Session) restoreIncrementalAdmission(
	admission *incrementalOutputAdmission,
) error {
	if admission == nil {
		return nil
	}
	if session == nil || admission.selection.Identity().IsZero() || admission.intentDigest.IsZero() ||
		admission.rootBinding.IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	root, rootErr := resumestate.FileCheckpointRootIDFromBytes(admission.rootBinding.Bytes())
	if rootErr != nil || session.checkpointRuntime.RootIdentity != root ||
		session.selection.ShareInstance() != admission.selection.ShareInstance() ||
		session.selection.SyntheticRoot() != admission.selection.SyntheticRoot() ||
		session.selection.RootGeneration() != admission.selection.RootGeneration() ||
		admission.selection.DirectoryCount() != uint64(len(session.selectedDirs)) {
		return transfer.ErrInvalidOutputSelection
	}
	knownKeys := make(map[resumestate.LiveFileKey]struct{}, len(admission.files))
	for path, live := range admission.files {
		key, keyErr := live.Key()
		if keyErr != nil || path != live.Selection.Path || live.IntentDigest != admission.intentDigest {
			return fmt.Errorf("%w: incremental file ledger binding", resumestate.ErrInvalidState)
		}
		knownKeys[key] = struct{}{}
	}
	for key, checkpoint := range admission.checkpoints {
		_, known := knownKeys[key]
		deferred := !known && key.ParentToken == [sha256.Size]byte{} &&
			checkpointByPathMatches(admission.checkpointByPath, key.CanonicalPath, checkpoint)
		if !known && !deferred || checkpoint.TransferIntentDigest() != key.IntentDigest ||
			checkpoint.FileID() != key.FileID || checkpoint.FileRevision() != key.Revision ||
			checkpoint.CanonicalPath() != key.CanonicalPath || checkpoint.ExactSize() != key.ExactSize {
			return fmt.Errorf("%w: incremental checkpoint ledger binding", resumestate.ErrInvalidState)
		}
	}
	session.incrementalSelection = admission.selection
	session.incrementalAdmission = true
	session.incrementalIntentDigest = admission.intentDigest
	session.admittedDirs = maps.Clone(admission.admittedDirs)
	if session.admittedDirs == nil {
		session.admittedDirs = make(map[string]transfer.DirectoryAdmission)
	}
	session.incrementalFiles = maps.Clone(admission.files)
	session.incrementalCheckpoints = maps.Clone(admission.checkpoints)
	session.incrementalCheckpointByPath = maps.Clone(admission.checkpointByPath)
	if session.incrementalFiles == nil {
		session.incrementalFiles = make(map[string]resumestate.LiveFileSelection)
	}
	if session.incrementalCheckpoints == nil {
		session.incrementalCheckpoints = make(map[resumestate.LiveFileKey]resumestate.FileCheckpointV1)
	}
	if session.incrementalCheckpointByPath == nil {
		session.incrementalCheckpointByPath = make(map[string]resumestate.FileCheckpointV1)
	}
	return nil
}

func checkpointByPathMatches(
	byPath map[string]resumestate.FileCheckpointV1,
	path string,
	candidate resumestate.FileCheckpointV1,
) bool {
	current, found := byPath[path]
	return found && current.RecordID() == candidate.RecordID() &&
		current.TransferIntentDigest() == candidate.TransferIntentDigest() &&
		current.FileID() == candidate.FileID() && current.FileRevision() == candidate.FileRevision() &&
		current.CanonicalPath() == candidate.CanonicalPath() && current.ExactSize() == candidate.ExactSize()
}

// installIncrementalAdmission publishes one catalog generation as the live
// ancestry view. It updates only the in-memory authority projection;
// FileCheckpointV1 remains the restart authority for dynamic files. Callers hold
// the adapter ledger lock while invoking this method, but the Session lock still
// protects concurrent file and terminal operations that do not know about the
// adapter.
func (session *Session) installIncrementalAdmission(
	intentDigest transfer.TransferIntentDigest,
	selection transfer.OutputSelection,
	snapshot outputAncestrySnapshot,
	directory transfer.OutputDirectory,
	admission transfer.DirectoryAdmission,
) error {
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if intentDigest.IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.incrementalIntentDigest.IsZero() && session.incrementalIntentDigest != intentDigest {
		return transfer.ErrInvalidOutputSelection
	}
	session.incrementalIntentDigest = intentDigest
	if session.selectedDirs == nil {
		session.selectedDirs = make(map[string]transfer.OutputSelectionDirectory)
	}
	if session.admittedDirs == nil {
		session.admittedDirs = make(map[string]transfer.DirectoryAdmission)
	}
	if directory.Path != "" {
		session.selectedDirs[directory.Path] = transfer.OutputSelectionDirectory{
			Path: directory.Path, DirectoryID: directory.DirectoryID,
			Generation: directory.Generation, ModifiedTime: directory.ModifiedTime,
		}
	}
	session.admittedDirs[directory.Path] = admission
	snapshot.entries = slices.Clone(snapshot.entries)
	session.incrementalSelection = selection
	session.incrementalAdmission = true
	session.ancestry = snapshot
	return nil
}

// installIncrementalFileSelection atomically extends the in-memory authority
// overlay. The frozen authority image is intentionally unchanged: dynamic files
// become recoverable only through a FileCheckpointV1, never through a widened
// selection map.
func (session *Session) installIncrementalFileSelection(
	live resumestate.LiveFileSelection,
) error {
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	if !session.incrementalAdmission || session.incrementalIntentDigest.IsZero() ||
		live.IntentDigest != session.incrementalIntentDigest {
		return transfer.ErrInvalidOutputSelection
	}
	if session.incrementalFiles == nil {
		session.incrementalFiles = make(map[string]resumestate.LiveFileSelection)
	}
	session.incrementalFiles[live.Selection.Path] = live
	if checkpoint, found := session.incrementalCheckpointByPath[live.Selection.Path]; found {
		key, keyErr := live.Key()
		if keyErr != nil {
			return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, keyErr)
		}
		if session.incrementalCheckpoints == nil {
			session.incrementalCheckpoints = make(map[resumestate.LiveFileKey]resumestate.FileCheckpointV1)
		}
		// FileCheckpointV1 intentionally omits the parent receipt: the receipt is
		// process-local and must be freshly presented by BeginFile after restart.
		// Replace any deferred zero-parent ledger key once that receipt is bound;
		// retaining both keys would make one authenticated file appear duplicated at
		// terminal settlement.
		for existingKey := range session.incrementalCheckpoints {
			if existingKey.IntentDigest == key.IntentDigest && existingKey.FileID == key.FileID &&
				existingKey.Revision == key.Revision && existingKey.CanonicalPath == key.CanonicalPath &&
				existingKey.ExactSize == key.ExactSize && existingKey != key {
				delete(session.incrementalCheckpoints, existingKey)
			}
		}
		session.incrementalCheckpoints[key] = checkpoint
	}
	return nil
}

func (session *Session) incrementalFileSelection(path string) (resumestate.LiveFileSelection, bool) {
	if session == nil {
		return resumestate.LiveFileSelection{}, false
	}
	session.stateInstall.RLock()
	defer session.stateInstall.RUnlock()
	live, found := session.incrementalFiles[path]
	return live, found
}
