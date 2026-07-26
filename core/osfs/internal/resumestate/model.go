// Package resumestate defines the platform-neutral durable state used by the
// native filesystem output backend. It deliberately models identity and crash
// recovery without persisting inode numbers or Windows file IDs.
package resumestate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	SchemaVersion                 = uint32(3)
	OutputObjectIDBytes           = sha256.Size
	OutputRootBindingBytes        = sha256.Size
	OutputAncestryBindingBytes    = sha256.Size
	BootstrapNonceBytes           = sha256.Size
	MaxRootIdentityClaimBytes     = 256
	MaxAncestryIdentityClaimBytes = 256
	MaxDurableRangesPerFile       = 16_384
	MaxFilesPerSession            = 1_048_576
	MaxSelectedEntriesPerSession  = 1_048_576
	MaxSessionsPerIntent          = 64
)

var (
	ErrInvalidState      = errors.New("osfs resumestate value is invalid")
	ErrCorruptState      = errors.New("osfs resumestate record is corrupt")
	ErrInvalidTransition = errors.New("osfs resumestate transition is invalid")
)

type (
	// OutputObjectID owns an internal namespace; current-object same-file checks,
	// rather than these random bytes, prove that stage, anchor, and final are links
	// to one live object.
	OutputObjectID [OutputObjectIDBytes]byte
	LocatorDigest  [sha256.Size]byte
	BootstrapNonce [BootstrapNonceBytes]byte
)

// OutputRootBinding is derived from both canonical volume and current-directory
// identity claims. The claims remain comparison hints, but binding both prevents
// copied control state from authenticating beneath a different root handle.
type OutputRootBinding struct {
	certification CertificationID
	digest        [OutputRootBindingBytes]byte
}

// OutputAncestryIdentityClaim is one exact native directory identity in the
// canonical root-to-selected-parent closure. The opaque claim is consumed only
// while deriving a binding; native identity material is never persisted.
type OutputAncestryIdentityClaim struct {
	CanonicalPath string
	IdentityClaim []byte
}

// OutputAncestryBinding commits to the complete ordered ancestry closure for
// one canonical selection. A fixed digest keeps the durable header bounded even
// when the selection approaches its admission limit.
type OutputAncestryBinding struct {
	digest [OutputAncestryBindingBytes]byte
}

func NewOutputAncestryBinding(
	root OutputRootBinding,
	selection transfer.SelectionIdentity,
	claims []OutputAncestryIdentityClaim,
) (OutputAncestryBinding, error) {
	if !root.valid() || selection.IsZero() || len(claims) == 0 ||
		len(claims) > MaxSelectedEntriesPerSession+1 {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding inputs", ErrInvalidState)
	}
	if claims[0].CanonicalPath != "" {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry root claim", ErrInvalidState)
	}
	hash := sha256.New()
	writeAncestryBindingBytes(hash, []byte("windshare/output-ancestry-binding/v3"))
	writeAncestryBindingBytes(hash, root.Bytes())
	writeAncestryBindingBytes(hash, selection.Bytes())
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(claims)))
	_, _ = hash.Write(count[:])
	for index, claim := range claims {
		canonical := claim.CanonicalPath
		if canonical == "" {
			if index != 0 {
				return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry root ordering", ErrInvalidState)
			}
		} else {
			validated, err := catalog.CanonicalPath(canonical)
			if err != nil || validated != canonical {
				return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry canonical path", ErrInvalidState)
			}
		}
		if index > 0 && claims[index-1].CanonicalPath >= canonical {
			return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry order or duplicate", ErrInvalidState)
		}
		if len(claim.IdentityClaim) == 0 || len(claim.IdentityClaim) > MaxAncestryIdentityClaimBytes {
			return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry identity claim", ErrInvalidState)
		}
		writeAncestryBindingBytes(hash, []byte(canonical))
		writeAncestryBindingBytes(hash, claim.IdentityClaim)
	}
	var digest [OutputAncestryBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputAncestryBindingBytes]byte{}) {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding is zero", ErrInvalidState)
	}
	return OutputAncestryBinding{digest: digest}, nil
}

func outputAncestryBindingFromBytes(raw []byte) (OutputAncestryBinding, error) {
	if len(raw) != OutputAncestryBindingBytes {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding", ErrInvalidState)
	}
	var digest [OutputAncestryBindingBytes]byte
	copy(digest[:], raw)
	if digest == ([OutputAncestryBindingBytes]byte{}) {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding is zero", ErrInvalidState)
	}
	return OutputAncestryBinding{digest: digest}, nil
}

func writeAncestryBindingBytes(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func (binding OutputAncestryBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}
func (binding OutputAncestryBinding) String() string { return hex.EncodeToString(binding.digest[:]) }
func (binding OutputAncestryBinding) IsZero() bool {
	return binding.digest == ([OutputAncestryBindingBytes]byte{})
}
func (binding OutputAncestryBinding) valid() bool { return !binding.IsZero() }

func NewOutputRootBinding(
	certification CertificationID,
	volumeIdentity []byte,
	objectIdentity []byte,
) (OutputRootBinding, error) {
	validatedCertification, certificationErr := NewCertificationID(string(certification))
	if certificationErr != nil || len(volumeIdentity) == 0 || len(volumeIdentity) > MaxRootIdentityClaimBytes ||
		len(objectIdentity) == 0 || len(objectIdentity) > MaxRootIdentityClaimBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: output root identity claims", ErrInvalidState)
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte("windshare/output-root-binding/v3"))
	writeRootBindingClaim(hash, []byte(validatedCertification))
	writeRootBindingClaim(hash, volumeIdentity)
	writeRootBindingClaim(hash, objectIdentity)
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding is zero", ErrInvalidState)
	}
	return OutputRootBinding{certification: validatedCertification, digest: digest}, nil
}

func outputRootBindingFromBytes(
	certification CertificationID,
	raw []byte,
) (OutputRootBinding, error) {
	validatedCertification, certificationErr := NewCertificationID(string(certification))
	if certificationErr != nil || len(raw) != OutputRootBindingBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding", ErrInvalidState)
	}
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], raw)
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding is zero", ErrInvalidState)
	}
	return OutputRootBinding{certification: validatedCertification, digest: digest}, nil
}

func writeRootBindingClaim(writer io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func (binding OutputRootBinding) Certification() CertificationID { return binding.certification }
func (binding OutputRootBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}
func (binding OutputRootBinding) String() string { return hex.EncodeToString(binding.digest[:]) }
func (binding OutputRootBinding) IsZero() bool {
	return binding.certification == "" && binding.digest == ([OutputRootBindingBytes]byte{})
}

func (binding OutputRootBinding) valid() bool {
	_, err := NewCertificationID(string(binding.certification))
	return err == nil && binding.digest != ([OutputRootBindingBytes]byte{})
}

func NewOutputObjectID() (OutputObjectID, error) {
	return GenerateOutputObjectID(rand.Reader)
}

func NewBootstrapNonce() (BootstrapNonce, error) {
	return GenerateBootstrapNonce(rand.Reader)
}

// GenerateOutputObjectID accepts an injected entropy source so allocation
// failures and collisions can be tested. Exclusive namespace creation remains
// the final collision arbiter even when the source is cryptographically secure.
func GenerateOutputObjectID(random io.Reader) (OutputObjectID, error) {
	var id OutputObjectID
	if random == nil {
		return id, fmt.Errorf("%w: output object entropy source is nil", ErrInvalidState)
	}
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return OutputObjectID{}, fmt.Errorf("generate output object ID: %w", err)
	}
	if id.IsZero() {
		return OutputObjectID{}, fmt.Errorf("%w: output object ID is zero", ErrInvalidState)
	}
	return id, nil
}

func GenerateBootstrapNonce(random io.Reader) (BootstrapNonce, error) {
	var nonce BootstrapNonce
	if random == nil {
		return nonce, fmt.Errorf("%w: bootstrap entropy source is nil", ErrInvalidState)
	}
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return BootstrapNonce{}, fmt.Errorf("generate bootstrap nonce: %w", err)
	}
	if nonce.IsZero() {
		return BootstrapNonce{}, fmt.Errorf("%w: bootstrap nonce is zero", ErrInvalidState)
	}
	return nonce, nil
}

func OutputObjectIDFromBytes(raw []byte) (OutputObjectID, error) {
	return fixedIDFromBytes[OutputObjectID](raw, OutputObjectIDBytes, "output object")
}

func LocatorDigestFromBytes(raw []byte) (LocatorDigest, error) {
	return fixedIDFromBytes[LocatorDigest](raw, sha256.Size, "locator digest")
}

func BootstrapNonceFromBytes(raw []byte) (BootstrapNonce, error) {
	return fixedIDFromBytes[BootstrapNonce](raw, BootstrapNonceBytes, "bootstrap nonce")
}

func fixedIDFromBytes[T ~[sha256.Size]byte](raw []byte, size int, name string) (T, error) {
	var id T
	if len(raw) != size {
		return id, fmt.Errorf("%w: %s ID has %d bytes", ErrInvalidState, name, len(raw))
	}
	copy(id[:], raw)
	var zero T
	if id == zero {
		return zero, fmt.Errorf("%w: %s ID is zero", ErrInvalidState, name)
	}
	return id, nil
}

func fixedIDBytes[T ~[sha256.Size]byte](id T) []byte  { return append([]byte(nil), id[:]...) }
func fixedIDString[T ~[sha256.Size]byte](id T) string { return hex.EncodeToString(id[:]) }

func (id OutputObjectID) Bytes() []byte { return fixedIDBytes(id) }
func (id LocatorDigest) Bytes() []byte  { return fixedIDBytes(id) }
func (id BootstrapNonce) Bytes() []byte { return fixedIDBytes(id) }

func (id OutputObjectID) String() string { return fixedIDString(id) }
func (id LocatorDigest) String() string  { return fixedIDString(id) }
func (id BootstrapNonce) String() string { return fixedIDString(id) }

func (id OutputObjectID) IsZero() bool { return id == OutputObjectID{} }
func (id LocatorDigest) IsZero() bool  { return id == LocatorDigest{} }
func (id BootstrapNonce) IsZero() bool { return id == BootstrapNonce{} }

type SessionLifecycle uint8

const (
	SessionActive               SessionLifecycle = 1
	SessionPausing              SessionLifecycle = 2
	SessionPaused               SessionLifecycle = 3
	SessionPausedNeedsAttention SessionLifecycle = 4
	SessionCompleting           SessionLifecycle = 5
	SessionDiscarding           SessionLifecycle = 6
)

func (lifecycle SessionLifecycle) Valid() bool {
	return lifecycle >= SessionActive && lifecycle <= SessionDiscarding
}

func (lifecycle SessionLifecycle) String() string {
	switch lifecycle {
	case SessionActive:
		return "active"
	case SessionPausing:
		return "pausing"
	case SessionPaused:
		return "paused"
	case SessionPausedNeedsAttention:
		return "paused-needs-attention"
	case SessionCompleting:
		return "completing"
	case SessionDiscarding:
		return "discarding"
	default:
		return "invalid"
	}
}

func CanTransitionSession(from, to SessionLifecycle) bool {
	switch from {
	case SessionActive:
		return to == SessionPausing || to == SessionCompleting || to == SessionDiscarding
	case SessionPausing:
		return to == SessionPaused || to == SessionPausedNeedsAttention
	case SessionPaused:
		return to == SessionActive || to == SessionDiscarding
	case SessionPausedNeedsAttention:
		return to == SessionActive || to == SessionDiscarding
	case SessionCompleting:
		return to == SessionPausedNeedsAttention
	default:
		return false
	}
}

type HeaderSpec struct {
	Backend        transfer.OutputBackendID
	SessionID      transfer.OutputSessionID
	Selection      transfer.OutputSelection
	OutputRoot     OutputRootBinding
	OutputAncestry OutputAncestryBinding
}

type Header struct {
	backend                transfer.OutputBackendID
	sessionID              transfer.OutputSessionID
	shareInstance          catalog.ShareInstance
	syntheticRoot          catalog.DirectoryID
	resumeIntent           transfer.ResumeIntent
	selectionIdentity      transfer.SelectionIdentity
	selectedDirectoryCount uint32
	selectedFileCount      uint32
	outputRoot             OutputRootBinding
	outputAncestry         OutputAncestryBinding
	lifecycle              SessionLifecycle
	stateGeneration        uint64
}

func NewHeader(spec HeaderSpec) (Header, error) {
	directories := spec.Selection.Directories()
	files := spec.Selection.Files()
	if len(files) > MaxFilesPerSession || len(directories)+len(files) > MaxSelectedEntriesPerSession {
		return Header{}, fmt.Errorf("%w: selected plan exceeds session bound", ErrInvalidState)
	}
	return newHeaderFromClaims(headerClaims{
		backend: spec.Backend, sessionID: spec.SessionID,
		shareInstance: spec.Selection.ShareInstance(), syntheticRoot: spec.Selection.SyntheticRoot(),
		resumeIntent: spec.Selection.ResumeIntent(), selectionIdentity: spec.Selection.Identity(),
		selectedDirectoryCount: uint32(len(directories)), selectedFileCount: uint32(len(files)),
		outputRoot: spec.OutputRoot, outputAncestry: spec.OutputAncestry,
		lifecycle: SessionActive, stateGeneration: 1,
	})
}

type headerClaims struct {
	backend                transfer.OutputBackendID
	sessionID              transfer.OutputSessionID
	shareInstance          catalog.ShareInstance
	syntheticRoot          catalog.DirectoryID
	resumeIntent           transfer.ResumeIntent
	selectionIdentity      transfer.SelectionIdentity
	selectedDirectoryCount uint32
	selectedFileCount      uint32
	outputRoot             OutputRootBinding
	outputAncestry         OutputAncestryBinding
	lifecycle              SessionLifecycle
	stateGeneration        uint64
}

func newHeaderFromClaims(claims headerClaims) (Header, error) {
	_, backendErr := transfer.NewOutputBackendID(string(claims.backend))
	selectedCount := uint64(claims.selectedDirectoryCount) + uint64(claims.selectedFileCount)
	if backendErr != nil || claims.sessionID.IsZero() || claims.shareInstance.IsZero() || claims.syntheticRoot.IsZero() ||
		claims.resumeIntent.IsZero() || claims.selectionIdentity.IsZero() || !claims.outputRoot.valid() ||
		!claims.outputAncestry.valid() ||
		!claims.lifecycle.Valid() || claims.stateGeneration == 0 ||
		uint64(claims.selectedFileCount) > MaxFilesPerSession || selectedCount > MaxSelectedEntriesPerSession {
		return Header{}, fmt.Errorf("%w: session header", ErrInvalidState)
	}
	return Header{
		backend: claims.backend, sessionID: claims.sessionID, shareInstance: claims.shareInstance,
		syntheticRoot: claims.syntheticRoot, resumeIntent: claims.resumeIntent,
		selectionIdentity:      claims.selectionIdentity,
		selectedDirectoryCount: claims.selectedDirectoryCount, selectedFileCount: claims.selectedFileCount,
		outputRoot: claims.outputRoot, outputAncestry: claims.outputAncestry,
		lifecycle: claims.lifecycle, stateGeneration: claims.stateGeneration,
	}, nil
}

func (header Header) Backend() transfer.OutputBackendID    { return header.backend }
func (header Header) SessionID() transfer.OutputSessionID  { return header.sessionID }
func (header Header) ShareInstance() catalog.ShareInstance { return header.shareInstance }
func (header Header) SyntheticRoot() catalog.DirectoryID   { return header.syntheticRoot }
func (header Header) ResumeIntent() transfer.ResumeIntent  { return header.resumeIntent }
func (header Header) SelectionIdentity() transfer.SelectionIdentity {
	return header.selectionIdentity
}
func (header Header) SelectedDirectoryCount() uint32         { return header.selectedDirectoryCount }
func (header Header) SelectedFileCount() uint32              { return header.selectedFileCount }
func (header Header) OutputRoot() OutputRootBinding          { return header.outputRoot }
func (header Header) OutputAncestry() OutputAncestryBinding  { return header.outputAncestry }
func (header Header) ResumeNamespace() transfer.ResumeIntent { return header.resumeIntent }
func (header Header) Lifecycle() SessionLifecycle            { return header.lifecycle }
func (header Header) StateGeneration() uint64                { return header.stateGeneration }

func (header Header) withLifecycle(next SessionLifecycle) (Header, error) {
	if !header.valid() || !CanTransitionSession(header.lifecycle, next) || header.stateGeneration == math.MaxUint64 {
		return Header{}, fmt.Errorf("%w: session %s -> %s", ErrInvalidTransition, header.lifecycle, next)
	}
	header.lifecycle = next
	header.stateGeneration++
	return header, nil
}

func (header Header) valid() bool {
	validated, err := newHeaderFromClaims(headerClaims{
		backend: header.backend, sessionID: header.sessionID, shareInstance: header.shareInstance,
		syntheticRoot: header.syntheticRoot, resumeIntent: header.resumeIntent,
		selectionIdentity:      header.selectionIdentity,
		selectedDirectoryCount: header.selectedDirectoryCount, selectedFileCount: header.selectedFileCount,
		outputRoot: header.outputRoot, outputAncestry: header.outputAncestry,
		lifecycle: header.lifecycle, stateGeneration: header.stateGeneration,
	})
	return err == nil && validated == header
}

type FilePhase uint8

const (
	FileReserved       FilePhase = 1
	FileWitnessed      FilePhase = 2
	FilePublishing     FilePhase = 3
	FilePublishBlocked FilePhase = 4
	FilePublished      FilePhase = 5
	FileRetiring       FilePhase = 6
	FileQuarantined    FilePhase = 7
)

func (phase FilePhase) Valid() bool { return phase >= FileReserved && phase <= FileQuarantined }

func (phase FilePhase) String() string {
	switch phase {
	case FileReserved:
		return "reserved"
	case FileWitnessed:
		return "witnessed"
	case FilePublishing:
		return "publishing"
	case FilePublishBlocked:
		return "publishBlocked"
	case FilePublished:
		return "published"
	case FileRetiring:
		return "retiring"
	case FileQuarantined:
		return "quarantined"
	default:
		return "invalid"
	}
}

type QuarantineReason uint8

const (
	QuarantineAnchorMissing         QuarantineReason = 1
	QuarantineAnchorUnsafe          QuarantineReason = 2
	QuarantineStageMissing          QuarantineReason = 3
	QuarantineStageMismatch         QuarantineReason = 4
	QuarantineStageUnsafe           QuarantineReason = 5
	QuarantineFinalMismatch         QuarantineReason = 6
	QuarantineFinalUnsafe           QuarantineReason = 7
	QuarantinePartialObjectCreation QuarantineReason = 8
	QuarantinePublicationHistory    QuarantineReason = 9
	QuarantineMetadataMismatch      QuarantineReason = 10
	QuarantineUpdateTemporary       QuarantineReason = 11
	QuarantineOutputObjectDuplicate QuarantineReason = 12
)

func (reason QuarantineReason) Valid() bool {
	return reason >= QuarantineAnchorMissing && reason <= QuarantineOutputObjectDuplicate
}

type RetirementReason uint8

const (
	RetirementPublished           RetirementReason = 1
	RetirementIsolatedFailure     RetirementReason = 2
	RetirementPreObjectCollision  RetirementReason = 3
	RetirementInvalidatedRevision RetirementReason = 4
)

func (reason RetirementReason) Valid() bool {
	return reason >= RetirementPublished && reason <= RetirementInvalidatedRevision
}

func CanTransitionFile(from, to FilePhase) bool {
	switch from {
	case FileReserved:
		return to == FileWitnessed || to == FileRetiring || to == FileQuarantined
	case FileWitnessed:
		return to == FilePublishing || to == FileRetiring || to == FileQuarantined
	case FilePublishing:
		return to == FilePublishBlocked || to == FilePublished || to == FileRetiring || to == FileQuarantined
	case FilePublishBlocked:
		return to == FilePublishing || to == FileRetiring || to == FileQuarantined
	case FilePublished:
		return to == FileRetiring || to == FileQuarantined
	case FileRetiring:
		return to == FileQuarantined
	default:
		return false
	}
}

type FileRecordSpec struct {
	Session          SessionAuthority
	Descriptor       content.FileRevisionDescriptor
	CanonicalLocator string
	OutputObject     OutputObjectID
}

type ExpectedMetadata struct {
	ModifiedTime catalog.ModifiedTime
}

type FileRecord struct {
	sessionID             transfer.OutputSessionID
	shareInstance         catalog.ShareInstance
	fileID                catalog.FileID
	revision              content.FileRevision
	canonicalLocator      string
	locatorDigest         LocatorDigest
	outputObject          OutputObjectID
	exactSize             uint64
	chunkSize             uint32
	stateGeneration       uint64
	checkpointGeneration  uint64
	durableRanges         content.RangeSet
	phase                 FilePhase
	quarantineReason      QuarantineReason
	phaseBeforeQuarantine FilePhase
	expectedMetadata      ExpectedMetadata
	retirementReason      RetirementReason
}

func NewFileRecord(spec FileRecordSpec) (ResumableFileAuthority, error) {
	if !spec.Session.valid() {
		return ResumableFileAuthority{}, fmt.Errorf("%w: file record session authority", ErrInvalidState)
	}
	canonical, err := catalog.CanonicalPath(spec.CanonicalLocator)
	selected, found := spec.Session.selectedFile(canonical)
	header := spec.Session.Header()
	if err != nil || canonical != spec.CanonicalLocator || !found || spec.Descriptor.ShareInstance() != header.shareInstance ||
		spec.Descriptor.FileID() != selected.FileID || spec.Descriptor.ExactSize() != selected.ExpectedSize ||
		spec.Descriptor.ModifiedTime() != selected.ModifiedTime {
		return ResumableFileAuthority{}, fmt.Errorf("%w: file record descriptor binding", ErrInvalidState)
	}
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: header.sessionID, shareInstance: spec.Descriptor.ShareInstance(),
		fileID: spec.Descriptor.FileID(), revision: spec.Descriptor.FileRevision(),
		canonicalLocator: canonical, outputObject: spec.OutputObject, exactSize: spec.Descriptor.ExactSize(),
		chunkSize:       spec.Descriptor.Geometry().ChunkSize(),
		stateGeneration: 1, phase: FileReserved,
		expectedMetadata: ExpectedMetadata{ModifiedTime: spec.Descriptor.ModifiedTime()},
	})
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	name := FileRecordName(record.locatorDigest)
	bound, err := BindFileRecord(spec.Session, name.shard, name.name, record)
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	return BindResumableFile(bound, spec.Descriptor)
}

type fileRecordClaims struct {
	sessionID             transfer.OutputSessionID
	shareInstance         catalog.ShareInstance
	fileID                catalog.FileID
	revision              content.FileRevision
	canonicalLocator      string
	outputObject          OutputObjectID
	exactSize             uint64
	chunkSize             uint32
	stateGeneration       uint64
	checkpointGeneration  uint64
	durableRanges         content.RangeSet
	phase                 FilePhase
	quarantineReason      QuarantineReason
	phaseBeforeQuarantine FilePhase
	expectedMetadata      ExpectedMetadata
	retirementReason      RetirementReason
}

func newFileRecordFromClaims(claims fileRecordClaims) (FileRecord, error) {
	canonical, err := catalog.CanonicalPath(claims.canonicalLocator)
	_, geometryErr := content.NewFileGeometry(claims.exactSize, claims.chunkSize)
	if err != nil || geometryErr != nil || canonical != claims.canonicalLocator || claims.sessionID.IsZero() || claims.shareInstance.IsZero() ||
		claims.fileID.IsZero() || claims.revision.IsZero() || claims.outputObject.IsZero() ||
		!claims.phase.Valid() || claims.stateGeneration == 0 {
		return FileRecord{}, fmt.Errorf("%w: file record identity", ErrInvalidState)
	}
	// RangeSet.Ranges clones its backing slice, so enforce the persisted bound
	// before validation to keep hostile in-memory inputs from forcing an
	// unbounded allocation on the state path.
	if claims.durableRanges.Len() > MaxDurableRangesPerFile {
		return FileRecord{}, fmt.Errorf("%w: durable ranges", ErrInvalidState)
	}
	ranges, err := content.NewRangeSet(claims.durableRanges.Ranges())
	if err != nil {
		return FileRecord{}, fmt.Errorf("%w: durable ranges", ErrInvalidState)
	}
	locatorDigest := DigestCanonicalLocator(canonical)
	if locatorDigest.IsZero() {
		return FileRecord{}, fmt.Errorf("%w: canonical locator digest", ErrInvalidState)
	}
	record := FileRecord{
		sessionID: claims.sessionID, shareInstance: claims.shareInstance, fileID: claims.fileID,
		revision: claims.revision, canonicalLocator: canonical, locatorDigest: locatorDigest,
		outputObject: claims.outputObject, exactSize: claims.exactSize, stateGeneration: claims.stateGeneration,
		chunkSize:            claims.chunkSize,
		checkpointGeneration: claims.checkpointGeneration, durableRanges: ranges, phase: claims.phase,
		quarantineReason: claims.quarantineReason, phaseBeforeQuarantine: claims.phaseBeforeQuarantine,
		expectedMetadata: claims.expectedMetadata, retirementReason: claims.retirementReason,
	}
	if !record.validRangesAndPhase() {
		return FileRecord{}, fmt.Errorf("%w: file record phase or ranges", ErrInvalidState)
	}
	return record, nil
}

func DigestCanonicalLocator(canonical string) LocatorDigest {
	locator, err := transfer.NewPathOutputLocator(canonical)
	if err != nil || locator.CanonicalPath() != canonical {
		return LocatorDigest{}
	}
	return LocatorDigest(locator.Digest())
}

func (id LocatorDigest) OutputLocatorDigest() transfer.OutputLocatorDigest {
	return transfer.OutputLocatorDigest(id)
}

func (record FileRecord) SessionID() transfer.OutputSessionID  { return record.sessionID }
func (record FileRecord) ShareInstance() catalog.ShareInstance { return record.shareInstance }
func (record FileRecord) FileID() catalog.FileID               { return record.fileID }
func (record FileRecord) Revision() content.FileRevision       { return record.revision }
func (record FileRecord) CanonicalLocator() string             { return record.canonicalLocator }
func (record FileRecord) LocatorDigest() LocatorDigest         { return record.locatorDigest }
func (record FileRecord) OutputObject() OutputObjectID         { return record.outputObject }
func (record FileRecord) ExactSize() uint64                    { return record.exactSize }
func (record FileRecord) ChunkSize() uint32                    { return record.chunkSize }
func (record FileRecord) StateGeneration() uint64              { return record.stateGeneration }
func (record FileRecord) CheckpointGeneration() uint64         { return record.checkpointGeneration }
func (record FileRecord) Phase() FilePhase                     { return record.phase }
func (record FileRecord) QuarantineReason() QuarantineReason   { return record.quarantineReason }
func (record FileRecord) PhaseBeforeQuarantine() FilePhase     { return record.phaseBeforeQuarantine }
func (record FileRecord) ExpectedMetadata() ExpectedMetadata   { return record.expectedMetadata }
func (record FileRecord) RetirementReason() RetirementReason   { return record.retirementReason }
func (record FileRecord) DurableRanges() content.RangeSet {
	ranges, _ := content.NewRangeSet(record.durableRanges.Ranges())
	return ranges
}

func (record FileRecord) Complete() bool {
	return rangesCoverFile(record.exactSize, record.durableRanges)
}

func (record FileRecord) withCheckpoint(generation uint64, ranges content.RangeSet) (FileRecord, error) {
	if !record.valid() || record.phase != FileWitnessed || record.checkpointGeneration == math.MaxUint64 ||
		record.stateGeneration == math.MaxUint64 || generation != record.checkpointGeneration+1 {
		return FileRecord{}, fmt.Errorf("%w: checkpoint generation", ErrInvalidTransition)
	}
	if ranges.Len() > MaxDurableRangesPerFile {
		return FileRecord{}, fmt.Errorf("%w: checkpoint ranges", ErrInvalidTransition)
	}
	validated, err := content.NewRangeSet(ranges.Ranges())
	if err != nil || validated.IsEmpty() ||
		!rangesContain(validated, record.durableRanges) || equalRanges(validated, record.durableRanges) {
		return FileRecord{}, fmt.Errorf("%w: checkpoint ranges", ErrInvalidTransition)
	}
	for _, current := range validated.Ranges() {
		if current.End > record.exactSize {
			return FileRecord{}, fmt.Errorf("%w: checkpoint exceeds exact size", ErrInvalidTransition)
		}
	}
	record.stateGeneration++
	record.checkpointGeneration = generation
	record.durableRanges = validated
	return record, nil
}

type FileTransition struct {
	Next             FilePhase
	RetirementReason RetirementReason
	QuarantineReason QuarantineReason
}

func (record FileRecord) transition(transition FileTransition) (FileRecord, error) {
	next := transition.Next
	if !record.valid() || !CanTransitionFile(record.phase, next) || record.stateGeneration == math.MaxUint64 ||
		(next == FileQuarantined) != transition.QuarantineReason.Valid() ||
		(next == FileRetiring) != transition.RetirementReason.Valid() ||
		next == FileRetiring && !validRetirementAuthority(record.phase, transition.RetirementReason) {
		return FileRecord{}, fmt.Errorf("%w: file %s -> %s", ErrInvalidTransition, record.phase, next)
	}
	previous := record.phase
	record.phase = next
	record.quarantineReason = transition.QuarantineReason
	record.phaseBeforeQuarantine = 0
	if next == FileRetiring {
		record.retirementReason = transition.RetirementReason
	}
	if next == FileQuarantined {
		record.phaseBeforeQuarantine = previous
	}
	record.stateGeneration++
	if !record.validRangesAndPhase() {
		return FileRecord{}, fmt.Errorf("%w: phase requirements", ErrInvalidTransition)
	}
	return record, nil
}

func validRetirementAuthority(from FilePhase, reason RetirementReason) bool {
	switch reason {
	case RetirementPublished:
		return from == FilePublished
	case RetirementIsolatedFailure:
		return from == FileReserved || from == FileWitnessed || from == FilePublishBlocked
	case RetirementPreObjectCollision:
		return from == FileReserved
	case RetirementInvalidatedRevision:
		return from >= FileReserved && from <= FilePublished
	default:
		return false
	}
}

func (record FileRecord) valid() bool {
	rebuilt, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: record.sessionID, shareInstance: record.shareInstance, fileID: record.fileID,
		revision: record.revision, canonicalLocator: record.canonicalLocator, outputObject: record.outputObject,
		exactSize: record.exactSize, chunkSize: record.chunkSize, stateGeneration: record.stateGeneration,
		checkpointGeneration: record.checkpointGeneration, durableRanges: record.durableRanges,
		phase: record.phase, quarantineReason: record.quarantineReason,
		phaseBeforeQuarantine: record.phaseBeforeQuarantine, expectedMetadata: record.expectedMetadata,
		retirementReason: record.retirementReason,
	})
	return err == nil && rebuilt.locatorDigest == record.locatorDigest
}

func (record FileRecord) validRangesAndPhase() bool {
	if (record.phase == FileQuarantined) != record.quarantineReason.Valid() ||
		(record.phase == FileQuarantined) != record.phaseBeforeQuarantine.Valid() ||
		record.phaseBeforeQuarantine == FileQuarantined {
		return false
	}
	if record.phase == FileQuarantined &&
		!validQuarantineHistory(record.phaseBeforeQuarantine, record.quarantineReason) {
		return false
	}
	retiringHistory := record.phase == FileRetiring ||
		record.phase == FileQuarantined && record.phaseBeforeQuarantine == FileRetiring
	if retiringHistory != record.retirementReason.Valid() {
		return false
	}
	if record.checkpointGeneration == 0 && !record.durableRanges.IsEmpty() ||
		record.checkpointGeneration > 0 && record.durableRanges.IsEmpty() ||
		record.exactSize == 0 && record.checkpointGeneration != 0 ||
		record.stateGeneration <= record.checkpointGeneration {
		return false
	}
	for _, current := range record.durableRanges.Ranges() {
		if current.End > record.exactSize {
			return false
		}
	}
	semanticPhase := record.phase
	if semanticPhase == FileQuarantined {
		semanticPhase = record.phaseBeforeQuarantine
	}
	if semanticPhase == FileReserved && (record.checkpointGeneration != 0 || !record.durableRanges.IsEmpty()) {
		return false
	}
	if semanticPhase == FilePublishing || semanticPhase == FilePublishBlocked || semanticPhase == FilePublished {
		return record.Complete()
	}
	if semanticPhase == FileRetiring {
		switch record.retirementReason {
		case RetirementPublished:
			return record.Complete()
		case RetirementPreObjectCollision:
			return record.checkpointGeneration == 0 && record.durableRanges.IsEmpty()
		case RetirementIsolatedFailure:
			return true
		case RetirementInvalidatedRevision:
			return true
		default:
			return false
		}
	}
	return true
}

func validQuarantineHistory(phase FilePhase, reason QuarantineReason) bool {
	switch reason {
	case QuarantineAnchorMissing:
		return phase == FileWitnessed || phase == FilePublishing || phase == FilePublishBlocked ||
			phase == FilePublished
	case QuarantineAnchorUnsafe, QuarantineStageUnsafe, QuarantineUpdateTemporary,
		QuarantineOutputObjectDuplicate:
		return phase >= FileReserved && phase <= FileRetiring
	case QuarantineStageMissing:
		return phase == FileWitnessed || phase == FilePublishing || phase == FilePublishBlocked
	case QuarantineStageMismatch:
		return phase >= FileReserved && phase <= FileRetiring
	case QuarantineFinalMismatch:
		return phase == FilePublished
	case QuarantineFinalUnsafe:
		return phase >= FileReserved && phase <= FilePublished
	case QuarantinePartialObjectCreation:
		return phase == FileReserved || phase == FileRetiring
	case QuarantinePublicationHistory:
		return phase == FileReserved || phase == FileWitnessed || phase == FilePublishing ||
			phase == FilePublishBlocked
	case QuarantineMetadataMismatch:
		return phase == FilePublishing || phase == FilePublished
	default:
		return false
	}
}

func rangesCoverFile(exactSize uint64, ranges content.RangeSet) bool {
	if exactSize == 0 {
		return ranges.IsEmpty()
	}
	items := ranges.Ranges()
	return len(items) == 1 && items[0] == (content.Range{Offset: 0, End: exactSize})
}

func equalRanges(left, right content.RangeSet) bool {
	leftRanges, rightRanges := left.Ranges(), right.Ranges()
	if len(leftRanges) != len(rightRanges) {
		return false
	}
	for index := range leftRanges {
		if leftRanges[index] != rightRanges[index] {
			return false
		}
	}
	return true
}

func rangesContain(container, contained content.RangeSet) bool {
	outer := container.Ranges()
	index := 0
	for _, inner := range contained.Ranges() {
		for index < len(outer) && outer[index].End < inner.End {
			index++
		}
		if index == len(outer) || outer[index].Offset > inner.Offset || outer[index].End < inner.End {
			return false
		}
	}
	return true
}
