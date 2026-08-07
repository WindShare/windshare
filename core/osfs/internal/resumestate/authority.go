package resumestate

import (
	"crypto/sha256"
	"fmt"
	"maps"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

// SessionNamespaceAuthority proves that a header belongs to the installed
// control object and to the exact intent/session names through which it was
// reached. Selection-independent operations such as explicit discard need this
// narrower proof; requiring a sender plan there would make recovery authority
// depend on data that listing deliberately does not retain.
type SessionNamespaceAuthority struct {
	control          Control
	header           Header
	intentDirectory  string
	sessionDirectory string
	bound            bool
}

func BindSessionNamespaceAuthority(
	control Control,
	header Header,
	intentDirectory string,
	sessionDirectory string,
) (SessionNamespaceAuthority, error) {
	if !control.valid() || !header.valid() || control.backend != header.backend || control.outputRoot != header.outputRoot {
		return SessionNamespaceAuthority{}, fmt.Errorf("%w: session namespace authority binding", ErrInvalidState)
	}
	intent, intentErr := ParseIntentNamespaceName(intentDirectory)
	session, sessionErr := ParseSessionDirectoryName(sessionDirectory)
	if intentErr != nil || sessionErr != nil || intent != header.intentDigest || session != header.sessionID {
		return SessionNamespaceAuthority{}, fmt.Errorf("%w: session namespace binding", ErrInvalidState)
	}
	return SessionNamespaceAuthority{
		control: control, header: header, intentDirectory: intentDirectory,
		sessionDirectory: sessionDirectory, bound: true,
	}, nil
}

func (authority SessionNamespaceAuthority) Control() Control { return authority.control }
func (authority SessionNamespaceAuthority) Header() Header   { return authority.header }
func (authority SessionNamespaceAuthority) IntentDirectory() string {
	return authority.intentDirectory
}
func (authority SessionNamespaceAuthority) SessionDirectory() string {
	return authority.sessionDirectory
}

func (authority SessionNamespaceAuthority) WithLifecycle(next SessionLifecycle) (SessionNamespaceAuthority, error) {
	if !authority.valid() {
		return SessionNamespaceAuthority{}, fmt.Errorf("%w: session lifecycle authority", ErrInvalidState)
	}
	header, err := authority.header.withLifecycle(next)
	if err != nil {
		return SessionNamespaceAuthority{}, err
	}
	authority.header = header
	return authority, nil
}

func (authority SessionNamespaceAuthority) valid() bool {
	if !authority.bound || !authority.control.valid() || !authority.header.valid() ||
		authority.control.backend != authority.header.backend ||
		authority.control.outputRoot != authority.header.outputRoot {
		return false
	}
	intent, intentErr := ParseIntentNamespaceName(authority.intentDirectory)
	session, sessionErr := ParseSessionDirectoryName(authority.sessionDirectory)
	return intentErr == nil && sessionErr == nil && intent == authority.header.intentDigest &&
		session == authority.header.sessionID
}

func (authority SessionNamespaceAuthority) empty() bool {
	return authority.control == (Control{}) && authority.header == (Header{}) &&
		authority.intentDirectory == "" && authority.sessionDirectory == "" && !authority.bound
}

// SessionAuthority adds the exact admitted canonical selection to the persisted
// namespace proof. Keeping this stronger type distinct prevents a syntactically
// valid orphan record from authorizing content I/O or automatic cleanup.
type SessionAuthority struct {
	namespace      SessionNamespaceAuthority
	selection      transfer.OutputSelection
	filesByLocator map[string]transfer.OutputSelectionFile
	// liveFilesByLocator is an in-process admission overlay for incremental
	// discovery. It is deliberately separate from filesByLocator: the latter is
	// reconstructed from the durable v3 header, while the overlay is accepted only
	// after a parent DirectoryAdmission and is never treated as restart authority.
	liveFilesByKey    map[LiveFileKey]LiveFileSelection
	liveKeysByLocator map[string]LiveFileKey
	liveIntentDigest  transfer.TransferIntentDigest
	bound             bool
}

// LiveFileSelection is the complete authority tuple for a file admitted after
// OpenOutput. The parent receipt is retained here (rather than inferred from a
// path) so a dynamic file cannot become writable when its directory generation
// changes. This value is process-local; a restart must rebuild it from a verified
// FileCheckpointV1 before any file record can be bound.
type LiveFileSelection struct {
	IntentDigest    transfer.TransferIntentDigest
	Selection       transfer.OutputSelectionFile
	Revision        content.FileRevision
	ParentAdmission transfer.DirectoryAdmission
}

// LiveFileKey makes every immutable binding component explicit. The parent token
// is represented by its fixed-size bytes so the key stays comparable and cannot
// accidentally depend on future private DirectoryAdmission fields.
type LiveFileKey struct {
	IntentDigest  transfer.TransferIntentDigest
	FileID        catalog.FileID
	Revision      content.FileRevision
	CanonicalPath string
	ExactSize     uint64
	ParentToken   [sha256.Size]byte
}

func (selection LiveFileSelection) Key() (LiveFileKey, error) {
	canonical, err := catalog.CanonicalPath(selection.Selection.Path)
	if err != nil || canonical != selection.Selection.Path || selection.IntentDigest.IsZero() ||
		selection.Selection.FileID.IsZero() || selection.Selection.ExpectedSize > catalog.MaxFileSize ||
		selection.Selection.ParentDirectoryID.IsZero() || selection.Selection.ParentGeneration.IsZero() ||
		selection.Revision.IsZero() || selection.ParentAdmission.IsZero() {
		return LiveFileKey{}, fmt.Errorf("%w: live file selection identity", ErrInvalidState)
	}
	var parentToken [sha256.Size]byte
	parentBytes := selection.ParentAdmission.Bytes()
	if len(parentBytes) != len(parentToken) {
		return LiveFileKey{}, fmt.Errorf("%w: live file parent admission", ErrInvalidState)
	}
	copy(parentToken[:], parentBytes)
	return LiveFileKey{
		IntentDigest: selection.IntentDigest, FileID: selection.Selection.FileID,
		Revision: selection.Revision, CanonicalPath: canonical,
		ExactSize: selection.Selection.ExpectedSize, ParentToken: parentToken,
	}, nil
}

func (selection LiveFileSelection) valid() bool {
	key, err := selection.Key()
	return err == nil && key.IntentDigest == selection.IntentDigest &&
		key.FileID == selection.Selection.FileID && key.Revision == selection.Revision &&
		key.CanonicalPath == selection.Selection.Path && key.ExactSize == selection.Selection.ExpectedSize
}

func BindSessionAuthority(
	control Control,
	header Header,
	selection transfer.OutputSelection,
	intentDirectory string,
	sessionDirectory string,
) (SessionAuthority, error) {
	namespace, err := BindSessionNamespaceAuthority(control, header, intentDirectory, sessionDirectory)
	if err != nil {
		return SessionAuthority{}, err
	}
	directories := selection.DirectoryCount()
	files := selection.FileCount()
	if selection.ShareInstance() != header.shareInstance || selection.SyntheticRoot() != header.syntheticRoot ||
		header.intentDigest.IsZero() || selection.Identity() != header.selectionIdentity ||
		directories != uint64(header.selectedDirectoryCount) ||
		files != uint64(header.selectedFileCount) {
		return SessionAuthority{}, fmt.Errorf("%w: selected plan binding", ErrInvalidState)
	}
	filesByLocator := make(map[string]transfer.OutputSelectionFile, int(files))
	if err := selection.VisitFiles(func(file transfer.OutputSelectionFile) error {
		filesByLocator[file.Path] = file
		return nil
	}); err != nil {
		return SessionAuthority{}, err
	}
	authority := SessionAuthority{
		namespace: namespace, selection: selection, filesByLocator: filesByLocator, bound: true,
	}
	if !authority.valid() {
		return SessionAuthority{}, fmt.Errorf("%w: session authority binding", ErrInvalidState)
	}
	return authority, nil
}

func (authority SessionAuthority) NamespaceAuthority() SessionNamespaceAuthority {
	return authority.namespace
}
func (authority SessionAuthority) Control() Control { return authority.namespace.control }
func (authority SessionAuthority) Header() Header   { return authority.namespace.header }
func (authority SessionAuthority) Selection() transfer.OutputSelection {
	return authority.selection
}
func (authority SessionAuthority) IntentDirectory() string {
	return authority.namespace.intentDirectory
}
func (authority SessionAuthority) SessionDirectory() string {
	return authority.namespace.sessionDirectory
}

func (authority SessionAuthority) WithLifecycle(next SessionLifecycle) (SessionAuthority, error) {
	if !authority.valid() {
		return SessionAuthority{}, fmt.Errorf("%w: session lifecycle authority", ErrInvalidState)
	}
	namespace, err := authority.namespace.WithLifecycle(next)
	if err != nil {
		return SessionAuthority{}, err
	}
	authority.namespace = namespace
	return authority, nil
}

// AuthorizedFileCount returns the exact number of immutable file authorities
// currently represented by this session. Incremental files are intentionally
// counted even though they are absent from the frozen v3 header: namespace
// enumeration must be bounded by authority that was actually admitted, never by
// a process-wide maximum or by untrusted names already present on disk.
func (authority SessionAuthority) AuthorizedFileCount() (uint32, error) {
	if !authority.valid() {
		return 0, fmt.Errorf("%w: authorized file count", ErrInvalidState)
	}
	count := uint64(len(authority.filesByLocator)) + uint64(len(authority.liveFilesByKey))
	if count > uint64(MaxFilesPerSession) {
		return 0, fmt.Errorf("%w: authorized file count", ErrFileStateNamespaceLimit)
	}
	return uint32(count), nil
}

func (authority SessionAuthority) valid() bool {
	header := authority.namespace.header
	if !authority.bound || !authority.namespace.valid() ||
		uint64(len(authority.filesByLocator)) != uint64(header.selectedFileCount) ||
		uint64(len(authority.filesByLocator))+uint64(len(authority.liveFilesByKey)) > uint64(MaxFilesPerSession) {
		return false
	}
	if authority.selection.ShareInstance() != header.shareInstance {
		return false
	}
	if authority.selection.SyntheticRoot() != header.syntheticRoot ||
		authority.selection.Identity() != header.selectionIdentity {
		return false
	}
	if len(authority.liveFilesByKey) != len(authority.liveKeysByLocator) {
		return false
	}
	if len(authority.liveFilesByKey) == 0 && !authority.liveIntentDigest.IsZero() {
		return false
	}
	for key, live := range authority.liveFilesByKey {
		if !live.valid() {
			return false
		}
		derived, err := live.Key()
		if err != nil || derived != key || authority.liveKeysByLocator[live.Selection.Path] != key ||
			(!authority.liveIntentDigest.IsZero() && authority.liveIntentDigest != live.IntentDigest) {
			return false
		}
	}
	return true
}

func (authority SessionAuthority) empty() bool {
	return authority.namespace.empty() && authority.selection.Identity().IsZero() &&
		authority.filesByLocator == nil &&
		authority.liveFilesByKey == nil && authority.liveKeysByLocator == nil &&
		authority.liveIntentDigest.IsZero() && !authority.bound
}

func (authority SessionAuthority) selectedFile(locator string) (transfer.OutputSelectionFile, bool) {
	if key, found := authority.liveKeysByLocator[locator]; found {
		live, liveFound := authority.liveFilesByKey[key]
		if liveFound {
			return live.Selection, true
		}
	}
	selected, found := authority.filesByLocator[locator]
	return selected, found
}

func (authority SessionAuthority) liveFile(locator string) (LiveFileSelection, bool) {
	key, found := authority.liveKeysByLocator[locator]
	if !found {
		return LiveFileSelection{}, false
	}
	live, found := authority.liveFilesByKey[key]
	return live, found
}

// WithLiveFileSelection returns a new authority carrying one dynamically admitted
// file. The immutable header and its frozen selection are untouched; this overlay
// therefore cannot accidentally become durable restart authority. A caller that
// wants to resume after a restart must first reconstruct the same tuple from a
// verified FileCheckpointV1.
func (authority SessionAuthority) WithLiveFileSelection(
	live LiveFileSelection,
) (SessionAuthority, error) {
	if !authority.valid() || !live.valid() {
		return SessionAuthority{}, fmt.Errorf("%w: live file selection authority", ErrInvalidState)
	}
	key, err := live.Key()
	if err != nil {
		return SessionAuthority{}, err
	}
	if existingStatic, exists := authority.filesByLocator[live.Selection.Path]; exists {
		if existingStatic != live.Selection {
			return SessionAuthority{}, fmt.Errorf("%w: live file shadows frozen selection", ErrInvalidState)
		}
		return SessionAuthority{}, fmt.Errorf("%w: live file path already selected", ErrInvalidState)
	}
	if existingKey, exists := authority.liveKeysByLocator[live.Selection.Path]; exists {
		if existingKey != key {
			return SessionAuthority{}, fmt.Errorf("%w: live file binding changed", ErrInvalidState)
		}
		return authority, nil
	}
	if !authority.liveIntentDigest.IsZero() && authority.liveIntentDigest != live.IntentDigest {
		return SessionAuthority{}, fmt.Errorf("%w: live file intent changed", ErrInvalidState)
	}
	if uint64(len(authority.filesByLocator))+uint64(len(authority.liveFilesByKey)) >= uint64(MaxFilesPerSession) {
		return SessionAuthority{}, fmt.Errorf("%w: live file selection authority", ErrFileStateNamespaceLimit)
	}
	if authority.liveFilesByKey == nil {
		authority.liveFilesByKey = make(map[LiveFileKey]LiveFileSelection)
	}
	if authority.liveKeysByLocator == nil {
		authority.liveKeysByLocator = make(map[string]LiveFileKey)
	}
	authority.liveFilesByKey = cloneLiveFileMap(authority.liveFilesByKey)
	authority.liveKeysByLocator = cloneLiveFileLocatorMap(authority.liveKeysByLocator)
	authority.liveFilesByKey[key] = live
	authority.liveKeysByLocator[live.Selection.Path] = key
	authority.liveIntentDigest = live.IntentDigest
	if !authority.valid() {
		return SessionAuthority{}, fmt.Errorf("%w: live file selection authority", ErrInvalidState)
	}
	return authority, nil
}

func cloneLiveFileMap(source map[LiveFileKey]LiveFileSelection) map[LiveFileKey]LiveFileSelection {
	if source == nil {
		return nil
	}
	clone := make(map[LiveFileKey]LiveFileSelection, len(source))
	maps.Copy(clone, source)
	return clone
}

func cloneLiveFileLocatorMap(source map[string]LiveFileKey) map[string]LiveFileKey {
	if source == nil {
		return nil
	}
	clone := make(map[string]LiveFileKey, len(source))
	maps.Copy(clone, source)
	return clone
}

// BoundFileRecord proves the whole authority chain down to the exact sharded
// record name and selected-file claim. Recovery reducers require this type
// because some of their decisions authorize namespace removal.
type BoundFileRecord struct {
	session           SessionAuthority
	checkpointRuntime CheckpointRuntimeBinding
	record            FileRecord
}

func BindFileRecord(
	session SessionAuthority,
	shard string,
	name string,
	record FileRecord,
) (BoundFileRecord, error) {
	header := session.Header()
	if !session.valid() || !record.valid() || record.sessionID != header.sessionID ||
		record.shareInstance != header.shareInstance {
		return BoundFileRecord{}, fmt.Errorf("%w: file authority binding", ErrInvalidState)
	}
	digest, err := ParseFileRecordName(shard, name)
	if err != nil || digest != record.locatorDigest {
		return BoundFileRecord{}, fmt.Errorf("%w: file record namespace binding", ErrInvalidState)
	}
	selected, found := session.selectedFile(record.canonicalLocator)
	if !found || selected.FileID != record.fileID || selected.ExpectedSize != record.exactSize ||
		selected.ModifiedTime != record.expectedMetadata.ModifiedTime {
		return BoundFileRecord{}, fmt.Errorf("%w: file record selection binding", ErrInvalidState)
	}
	if live, liveFound := session.liveFile(record.canonicalLocator); liveFound &&
		(live.IntentDigest != session.liveIntentDigest || live.Revision != record.revision ||
			live.Selection.FileID != record.fileID || live.Selection.ExpectedSize != record.exactSize) {
		return BoundFileRecord{}, fmt.Errorf("%w: live file revision binding", ErrInvalidState)
	}
	return BoundFileRecord{session: session, record: record}, nil
}

func (bound BoundFileRecord) Session() SessionAuthority { return bound.session }
func (bound BoundFileRecord) Record() FileRecord        { return bound.record }

func (bound BoundFileRecord) valid() bool {
	if !bound.record.valid() {
		return false
	}
	if bound.checkpointRuntime.valid() {
		return bound.checkpointRuntime.validRecord(bound.record)
	}
	if !bound.session.valid() {
		return false
	}
	name := FileRecordName(bound.record.locatorDigest)
	rebuilt, err := BindFileRecord(bound.session, name.shard, name.name, bound.record)
	return err == nil && rebuilt.record.locatorDigest == bound.record.locatorDigest
}

func (bound BoundFileRecord) transition(transition FileTransition) (BoundFileRecord, error) {
	if !bound.valid() {
		return BoundFileRecord{}, fmt.Errorf("%w: bound file transition", ErrInvalidState)
	}
	next, err := bound.record.transition(transition)
	if err != nil {
		return BoundFileRecord{}, err
	}
	bound.record = next
	return bound, nil
}

// PrepareUnsafeNamespaceQuarantine consumes bound record authority after current
// namespace evidence becomes unclassifiable. Keeping this transition explicit
// prevents a transient operational error from being misused as cleanup authority
// while still preserving the exact file namespace that must remain blocked.
func PrepareUnsafeNamespaceQuarantine(
	bound BoundFileRecord,
	reason QuarantineReason,
) (BoundFileRecord, error) {
	switch reason {
	case QuarantineAnchorUnsafe, QuarantineStageUnsafe, QuarantineFinalUnsafe,
		QuarantineUpdateTemporary, QuarantinePartialObjectCreation, QuarantinePublicationHistory:
	default:
		return BoundFileRecord{}, fmt.Errorf("%w: unsafe namespace quarantine reason", ErrInvalidTransition)
	}
	return bound.transition(FileTransition{Next: FileQuarantined, QuarantineReason: reason})
}

// ResumableFileAuthority adds the independently authenticated revision claim.
// Terminal recovery and cleanup need only BoundFileRecord; content resume and
// checkpoint advancement require this stronger proof.
type ResumableFileAuthority struct {
	bound      BoundFileRecord
	descriptor content.FileRevisionDescriptor
}

func BindResumableFile(
	bound BoundFileRecord,
	descriptor content.FileRevisionDescriptor,
) (ResumableFileAuthority, error) {
	if !bound.valid() || descriptor.ShareInstance() != bound.record.shareInstance ||
		descriptor.FileID() != bound.record.fileID || descriptor.FileRevision() != bound.record.revision ||
		descriptor.ExactSize() != bound.record.exactSize ||
		descriptor.Geometry().ChunkSize() != bound.record.chunkSize ||
		descriptor.ModifiedTime() != bound.record.expectedMetadata.ModifiedTime {
		return ResumableFileAuthority{}, fmt.Errorf("%w: resumable revision binding", ErrInvalidState)
	}
	return ResumableFileAuthority{bound: bound, descriptor: descriptor}, nil
}

func (authority ResumableFileAuthority) Bound() BoundFileRecord { return authority.bound }
func (authority ResumableFileAuthority) Descriptor() content.FileRevisionDescriptor {
	return authority.descriptor
}

func (authority ResumableFileAuthority) valid() bool {
	rebuilt, err := BindResumableFile(authority.bound, authority.descriptor)
	return err == nil && rebuilt.bound.record.locatorDigest == authority.bound.record.locatorDigest
}

func (authority ResumableFileAuthority) WithCheckpoint(
	generation uint64,
	ranges content.RangeSet,
) (ResumableFileAuthority, error) {
	if !authority.valid() {
		return ResumableFileAuthority{}, fmt.Errorf("%w: resumable file checkpoint", ErrInvalidState)
	}
	next, err := authority.bound.record.withCheckpoint(generation, ranges)
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	authority.bound.record = next
	return authority, nil
}

// PreparePublication creates the durable authorization record only after the
// caller has retained the independently bound revision and completed all
// ranges. The executor must still sync metadata/object state before installing
// the returned record.
func PreparePublication(authority ResumableFileAuthority) (BoundFileRecord, error) {
	if !authority.valid() || authority.bound.record.phase != FileWitnessed ||
		!authority.bound.record.Complete() {
		return BoundFileRecord{}, fmt.Errorf("%w: publication authority", ErrInvalidTransition)
	}
	return authority.bound.transition(FileTransition{Next: FilePublishing})
}

func PreparePublishedRetirement(bound BoundFileRecord) (BoundFileRecord, error) {
	if !bound.valid() || bound.record.phase != FilePublished {
		return BoundFileRecord{}, fmt.Errorf("%w: published retirement authority", ErrInvalidTransition)
	}
	return bound.transition(FileTransition{Next: FileRetiring, RetirementReason: RetirementPublished})
}

func PrepareIsolatedRetirement(bound BoundFileRecord) (BoundFileRecord, error) {
	if !bound.valid() {
		return BoundFileRecord{}, fmt.Errorf("%w: isolated retirement authority", ErrInvalidTransition)
	}
	return bound.transition(FileTransition{Next: FileRetiring, RetirementReason: RetirementIsolatedFailure})
}

// PrepareInvalidatedRevisionRetirement records why verified ranges are being
// discarded so a crash can never make a later revision inherit their authority.
func PrepareInvalidatedRevisionRetirement(bound BoundFileRecord) (BoundFileRecord, error) {
	if !bound.valid() {
		return BoundFileRecord{}, fmt.Errorf("%w: invalidated revision retirement authority", ErrInvalidTransition)
	}
	return bound.transition(FileTransition{Next: FileRetiring, RetirementReason: RetirementInvalidatedRevision})
}
