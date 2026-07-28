package resumestate

import (
	"fmt"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

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
