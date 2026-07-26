package resumestate

import (
	"fmt"

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
	intent, intentErr := ParseResumeNamespaceName(intentDirectory)
	session, sessionErr := ParseSessionDirectoryName(sessionDirectory)
	if intentErr != nil || sessionErr != nil || intent != header.resumeIntent || session != header.sessionID {
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
	intent, intentErr := ParseResumeNamespaceName(authority.intentDirectory)
	session, sessionErr := ParseSessionDirectoryName(authority.sessionDirectory)
	return intentErr == nil && sessionErr == nil && intent == authority.header.resumeIntent &&
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
	bound          bool
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
	directories := selection.Directories()
	files := selection.Files()
	if selection.ShareInstance() != header.shareInstance || selection.SyntheticRoot() != header.syntheticRoot ||
		selection.ResumeIntent() != header.resumeIntent || selection.Identity() != header.selectionIdentity ||
		uint64(len(directories)) != uint64(header.selectedDirectoryCount) ||
		uint64(len(files)) != uint64(header.selectedFileCount) {
		return SessionAuthority{}, fmt.Errorf("%w: selected plan binding", ErrInvalidState)
	}
	filesByLocator := make(map[string]transfer.OutputSelectionFile, len(files))
	for _, file := range files {
		filesByLocator[file.Path] = file
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

func (authority SessionAuthority) valid() bool {
	header := authority.namespace.header
	if !authority.bound || !authority.namespace.valid() ||
		uint64(len(authority.filesByLocator)) != uint64(header.selectedFileCount) {
		return false
	}
	return authority.selection.ShareInstance() == header.shareInstance &&
		authority.selection.SyntheticRoot() == header.syntheticRoot &&
		authority.selection.ResumeIntent() == header.resumeIntent &&
		authority.selection.Identity() == header.selectionIdentity
}

func (authority SessionAuthority) empty() bool {
	return authority.namespace.empty() && authority.selection.Identity().IsZero() &&
		authority.selection.ResumeIntent().IsZero() && authority.filesByLocator == nil && !authority.bound
}

func (authority SessionAuthority) selectedFile(locator string) (transfer.OutputSelectionFile, bool) {
	selected, found := authority.filesByLocator[locator]
	return selected, found
}

// BoundFileRecord proves the whole authority chain down to the exact sharded
// record name and selected-file claim. Recovery reducers require this type
// because some of their decisions authorize namespace removal.
type BoundFileRecord struct {
	session SessionAuthority
	record  FileRecord
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
	return BoundFileRecord{session: session, record: record}, nil
}

func (bound BoundFileRecord) Session() SessionAuthority { return bound.session }
func (bound BoundFileRecord) Record() FileRecord        { return bound.record }

func (bound BoundFileRecord) valid() bool {
	if !bound.session.valid() || !bound.record.valid() {
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
