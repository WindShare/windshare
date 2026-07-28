package resumestate

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

type RecoveryAction uint8

// RecoveryAction values are emitted through the output tracer. New actions
// append to this list so historical telemetry keeps a stable numeric meaning.
const (
	RecoveryRetryObjectCreation RecoveryAction = iota + 1
	RecoveryInstallWitness
	RecoveryRequireRevisionBinding
	RecoveryResumeContent
	RecoveryInstallPublishing
	RecoveryLinkFinalNoReplace
	RecoverySyncFinalParent
	RecoveryInstallPublished
	RecoveryInstallPublishBlocked
	RecoveryHoldPublishBlocked
	RecoveryRemovePublishedStageAndSync
	RecoverySyncPublishedStageParent
	RecoveryRemoveRetiringStageAndSync
	RecoverySyncStageRemoveAnchorAndSync
	RecoverySyncParentsRemoveRecordAndSync
	RecoveryInstallRetiring
	RecoveryInstallQuarantine
	RecoveryHoldQuarantine
	RecoveryHoldPublishedCleanup
	RecoveryHoldRetiringCleanup
)

type RecoverySettlement uint8

const (
	RecoveryContinuing RecoverySettlement = iota + 1
	RecoveryReadyForContent
	RecoveryCollision
	RecoveryPublishBlocked
	RecoveryPublished
	RecoveryRetiring
	RecoveryRetired
	RecoveryNeedsAttention
)

type RecoveryDecision struct {
	action           RecoveryAction
	settlement       RecoverySettlement
	nextPhase        FilePhase
	retirementReason RetirementReason
	quarantineReason QuarantineReason
	recordBinding    recoveryRecordBinding
	bound            bool
}

type recoveryRecordBinding struct {
	namespace             SessionNamespaceAuthority
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
	durableRangesDigest   [sha256.Size]byte
	phase                 FilePhase
	quarantineReason      QuarantineReason
	phaseBeforeQuarantine FilePhase
	metadata              ExpectedMetadata
	retirementReason      RetirementReason
}

func (decision RecoveryDecision) Action() RecoveryAction         { return decision.action }
func (decision RecoveryDecision) Settlement() RecoverySettlement { return decision.settlement }
func (decision RecoveryDecision) NextPhase() FilePhase           { return decision.nextPhase }
func (decision RecoveryDecision) RetirementReason() RetirementReason {
	return decision.retirementReason
}
func (decision RecoveryDecision) QuarantineReason() QuarantineReason {
	return decision.quarantineReason
}

// ApplyRecoveryDecision advances only the durable record described by a
// reducer decision. Namespace actions with no phase transition return the
// record unchanged.

func ApplyRecoveryDecision(bound BoundFileRecord, decision RecoveryDecision) (BoundFileRecord, error) {
	if !bound.valid() || !decision.bound || decision.recordBinding != recoveryBindingFor(bound) ||
		!decision.validFor(bound.record) {
		return BoundFileRecord{}, fmt.Errorf("%w: bound recovery record", ErrInvalidState)
	}
	if decision.nextPhase == 0 {
		return bound, nil
	}
	return bound.transition(FileTransition{
		Next: decision.nextPhase, RetirementReason: decision.retirementReason,
		QuarantineReason: decision.quarantineReason,
	})
}

// ReduceFileRecovery chooses exactly one durable or namespace step. It never
// performs I/O, so platform adapters can normalize current-object observations
// without leaking inode or File ID values into persistent state.
func ReduceFileRecovery(bound BoundFileRecord, observation FileObservation) (RecoveryDecision, error) {
	return reduceFileRecovery(bound, observation, false)
}

// ReduceResumableFileRecovery is the only reducer path that can release a
// witnessed record for content I/O because it also proves the later revision
// descriptor still matches the persisted independent binding.
func ReduceResumableFileRecovery(
	authority ResumableFileAuthority,
	observation FileObservation,
) (RecoveryDecision, error) {
	if !authority.valid() {
		return RecoveryDecision{}, fmt.Errorf("%w: resumable file authority", ErrInvalidState)
	}
	return reduceFileRecovery(authority.bound, observation, true)
}

func reduceFileRecovery(
	bound BoundFileRecord,
	observation FileObservation,
	resumeBound bool,
) (RecoveryDecision, error) {
	if !bound.valid() {
		return RecoveryDecision{}, fmt.Errorf("%w: bound file record", ErrInvalidState)
	}
	record := bound.record
	decision, err := reduceFileRecoveryRecord(record, observation, resumeBound)
	if err != nil {
		return RecoveryDecision{}, err
	}
	return bindRecoveryDecision(bound, decision), nil
}

func reduceFileRecoveryRecord(
	record FileRecord,
	observation FileObservation,
	resumeBound bool,
) (RecoveryDecision, error) {
	if record.phase == FileQuarantined {
		return recoveryDecision(RecoveryHoldQuarantine, RecoveryNeedsAttention, 0, 0, 0), nil
	}
	if observation.Final == EntryNotObserved {
		if reason, conclusive := internalFileObservationQuarantine(record.phase, observation); conclusive {
			return quarantine(reason), nil
		}
	}
	if err := validateObservation(record.phase, observation); err != nil {
		return RecoveryDecision{}, err
	}
	if record.phase == FileRetiring {
		return reduceRetiring(record, observation), nil
	}
	if record.phase == FilePublished {
		return reducePublished(observation), nil
	}
	if observation.Anchor == AnchorUnsafe {
		return quarantine(QuarantineAnchorUnsafe), nil
	}
	if observation.Anchor == AnchorMissing {
		return reduceWithoutAnchor(record, observation), nil
	}
	if observation.Stage == EntryUnsafe {
		return quarantine(QuarantineStageUnsafe), nil
	}
	if observation.Final == EntryUnsafe {
		return quarantine(QuarantineFinalUnsafe), nil
	}
	switch record.phase {
	case FileReserved:
		return reduceReservedWithAnchor(observation), nil
	case FileWitnessed:
		return reduceWitnessed(observation, resumeBound), nil
	case FilePublishing:
		return reducePublishing(observation), nil
	case FilePublishBlocked:
		return reducePublishBlocked(observation), nil
	default:
		return RecoveryDecision{}, fmt.Errorf("%w: file recovery phase", ErrInvalidState)
	}
}

func reduceWithoutAnchor(record FileRecord, observation FileObservation) RecoveryDecision {
	if record.phase == FileReserved {
		if observation.Stage == EntryMissing {
			switch observation.Final {
			case EntryMissing:
				return recoveryDecision(RecoveryRetryObjectCreation, RecoveryContinuing, 0, 0, 0)
			case EntryPresentUnresolved:
				return recoveryDecision(
					RecoveryInstallRetiring, RecoveryContinuing, FileRetiring, RetirementPreObjectCollision, 0,
				)
			case EntryUnsafe:
				return quarantine(QuarantineFinalUnsafe)
			}
		}
		if observation.Stage == EntryPresentUnresolved {
			return quarantine(QuarantinePartialObjectCreation)
		}
	}
	if observation.Stage == EntryUnsafe {
		return quarantine(QuarantineStageUnsafe)
	}
	return quarantine(QuarantineAnchorMissing)
}

func reduceReservedWithAnchor(observation FileObservation) RecoveryDecision {
	if observation.Stage == EntryMissing {
		return quarantine(QuarantinePartialObjectCreation)
	}
	if observation.Stage != EntrySameAsAnchor {
		return quarantine(QuarantineStageMismatch)
	}
	switch observation.Final {
	case EntrySameAsAnchor:
		return quarantine(QuarantinePublicationHistory)
	case EntryUnsafe:
		return quarantine(QuarantineFinalUnsafe)
	default:
		return recoveryDecision(RecoveryInstallWitness, RecoveryContinuing, FileWitnessed, 0, 0)
	}
}

func reduceWitnessed(observation FileObservation, resumeBound bool) RecoveryDecision {
	if observation.Stage == EntryMissing {
		return quarantine(QuarantineStageMissing)
	}
	if observation.Stage != EntrySameAsAnchor {
		return quarantine(QuarantineStageMismatch)
	}
	switch observation.Final {
	case EntryMissing, EntryDifferentFromAnchor:
		if !resumeBound {
			return recoveryDecision(RecoveryRequireRevisionBinding, RecoveryContinuing, 0, 0, 0)
		}
		return recoveryDecision(RecoveryResumeContent, RecoveryReadyForContent, 0, 0, 0)
	case EntrySameAsAnchor:
		return quarantine(QuarantinePublicationHistory)
	default:
		return quarantine(QuarantineFinalUnsafe)
	}
}

func reducePublishing(observation FileObservation) RecoveryDecision {
	switch observation.Final {
	case EntryMissing:
		if observation.Stage == EntryMissing {
			return quarantine(QuarantineStageMissing)
		}
		if observation.Stage != EntrySameAsAnchor {
			return quarantine(QuarantineStageMismatch)
		}
		return recoveryDecision(RecoveryLinkFinalNoReplace, RecoveryContinuing, 0, 0, 0)
	case EntrySameAsAnchor:
		if observation.Stage != EntryMissing && observation.Stage != EntrySameAsAnchor {
			return quarantine(QuarantineStageMismatch)
		}
		if observation.FinalParent != FinalParentSynced {
			return recoveryDecision(RecoverySyncFinalParent, RecoveryContinuing, 0, 0, 0)
		}
		if observation.Metadata != MetadataMatches {
			if observation.Metadata == MetadataDiffers {
				return quarantine(QuarantineMetadataMismatch)
			}
			return quarantine(QuarantineFinalUnsafe)
		}
		return recoveryDecision(RecoveryInstallPublished, RecoveryContinuing, FilePublished, 0, 0)
	case EntryDifferentFromAnchor:
		return quarantine(QuarantinePublicationHistory)
	default:
		return quarantine(QuarantineFinalUnsafe)
	}
}

func reducePublishBlocked(observation FileObservation) RecoveryDecision {
	if observation.Stage == EntryMissing {
		return quarantine(QuarantineStageMissing)
	}
	if observation.Stage != EntrySameAsAnchor {
		return quarantine(QuarantineStageMismatch)
	}
	switch observation.Final {
	case EntryMissing:
		return recoveryDecision(RecoveryInstallPublishing, RecoveryContinuing, FilePublishing, 0, 0)
	case EntryDifferentFromAnchor:
		return recoveryDecision(RecoveryHoldPublishBlocked, RecoveryPublishBlocked, 0, 0, 0)
	case EntrySameAsAnchor:
		return quarantine(QuarantinePublicationHistory)
	default:
		return quarantine(QuarantineFinalUnsafe)
	}
}

func reducePublished(observation FileObservation) RecoveryDecision {
	if observation.Final != EntrySameAsAnchor {
		if observation.Final == EntryUnsafe {
			return quarantine(QuarantineFinalUnsafe)
		}
		return quarantine(QuarantineFinalMismatch)
	}
	if observation.Metadata != MetadataMatches {
		if observation.Metadata == MetadataDiffers {
			return quarantine(QuarantineMetadataMismatch)
		}
		return quarantine(QuarantineFinalUnsafe)
	}
	switch observation.Stage {
	case EntrySameAsAnchor:
		return recoveryDecision(RecoveryRemovePublishedStageAndSync, RecoveryPublished, 0, 0, 0)
	case EntryMissing:
		return recoveryDecision(RecoverySyncPublishedStageParent, RecoveryPublished, 0, 0, 0)
	default:
		return recoveryDecision(RecoveryHoldPublishedCleanup, RecoveryNeedsAttention, 0, 0, 0)
	}
}

func reduceRetiring(record FileRecord, observation FileObservation) RecoveryDecision {
	switch observation.Anchor {
	case AnchorUnsafe:
		return recoveryDecision(RecoveryHoldRetiringCleanup, RecoveryNeedsAttention, 0, 0, 0)
	case AnchorMissing:
		switch observation.Stage {
		case EntryMissing:
			settlement := RecoveryRetired
			if record.retirementReason == RetirementPreObjectCollision {
				settlement = RecoveryCollision
			}
			return recoveryDecision(RecoverySyncParentsRemoveRecordAndSync, settlement, 0, 0, 0)
		default:
			return recoveryDecision(RecoveryHoldRetiringCleanup, RecoveryNeedsAttention, 0, 0, 0)
		}
	case AnchorVerified:
		switch observation.Stage {
		case EntrySameAsAnchor:
			return recoveryDecision(RecoveryRemoveRetiringStageAndSync, RecoveryRetiring, 0, 0, 0)
		case EntryMissing:
			return recoveryDecision(RecoverySyncStageRemoveAnchorAndSync, RecoveryRetiring, 0, 0, 0)
		default:
			return recoveryDecision(RecoveryHoldRetiringCleanup, RecoveryNeedsAttention, 0, 0, 0)
		}
	default:
		return recoveryDecision(RecoveryHoldRetiringCleanup, RecoveryNeedsAttention, 0, 0, 0)
	}
}

type PublishResult uint8

const (
	PublishLinkCreated PublishResult = iota + 1
	PublishAlreadyExistsDifferent
	PublishExistingAmbiguous
)

// ReducePublishResult keeps a direct no-replace collision distinct from an
// ambiguous foreign final observed after restart.
func ReducePublishResult(bound BoundFileRecord, result PublishResult) (RecoveryDecision, error) {
	if !bound.valid() || bound.record.phase != FilePublishing || !bound.record.Complete() {
		return RecoveryDecision{}, fmt.Errorf("%w: publish result record", ErrInvalidState)
	}
	switch result {
	case PublishLinkCreated:
		return bindRecoveryDecision(
			bound, recoveryDecision(RecoverySyncFinalParent, RecoveryContinuing, 0, 0, 0),
		), nil
	case PublishAlreadyExistsDifferent:
		return bindRecoveryDecision(bound, recoveryDecision(
			RecoveryInstallPublishBlocked, RecoveryPublishBlocked, FilePublishBlocked, 0, 0,
		)), nil
	case PublishExistingAmbiguous:
		return bindRecoveryDecision(bound, quarantine(QuarantinePublicationHistory)), nil
	default:
		return RecoveryDecision{}, fmt.Errorf("%w: publish result", ErrInvalidState)
	}
}

func quarantine(reason QuarantineReason) RecoveryDecision {
	return recoveryDecision(RecoveryInstallQuarantine, RecoveryNeedsAttention, FileQuarantined, 0, reason)
}

func recoveryDecision(
	action RecoveryAction,
	settlement RecoverySettlement,
	next FilePhase,
	retirement RetirementReason,
	quarantineReason QuarantineReason,
) RecoveryDecision {
	return RecoveryDecision{
		action: action, settlement: settlement, nextPhase: next,
		retirementReason: retirement, quarantineReason: quarantineReason,
	}
}

func bindRecoveryDecision(bound BoundFileRecord, decision RecoveryDecision) RecoveryDecision {
	decision.recordBinding = recoveryBindingFor(bound)
	decision.bound = true
	return decision
}

func recoveryBindingFor(bound BoundFileRecord) recoveryRecordBinding {
	record := bound.record
	hash := sha256.New()
	var encoded [16]byte
	for _, current := range record.durableRanges.Ranges() {
		binary.BigEndian.PutUint64(encoded[:8], current.Offset)
		binary.BigEndian.PutUint64(encoded[8:], current.End)
		_, _ = hash.Write(encoded[:])
	}
	var rangesDigest [sha256.Size]byte
	copy(rangesDigest[:], hash.Sum(nil))
	return recoveryRecordBinding{
		namespace: bound.session.namespace,
		sessionID: record.sessionID, shareInstance: record.shareInstance, fileID: record.fileID,
		revision: record.revision, canonicalLocator: record.canonicalLocator,
		locatorDigest: record.locatorDigest, outputObject: record.outputObject, exactSize: record.exactSize,
		chunkSize:       record.chunkSize,
		stateGeneration: record.stateGeneration, checkpointGeneration: record.checkpointGeneration,
		durableRangesDigest: rangesDigest, phase: record.phase, quarantineReason: record.quarantineReason,
		phaseBeforeQuarantine: record.phaseBeforeQuarantine, metadata: record.expectedMetadata,
		retirementReason: record.retirementReason,
	}
}

func (decision RecoveryDecision) validFor(record FileRecord) bool {
	if decision.action < RecoveryRetryObjectCreation || decision.action > RecoveryHoldRetiringCleanup {
		return false
	}
	if decision.action != RecoveryInstallRetiring && decision.retirementReason != 0 ||
		decision.action != RecoveryInstallQuarantine && decision.quarantineReason != 0 {
		return false
	}
	switch decision.action {
	case RecoveryInstallWitness:
		return decision.nextPhase == FileWitnessed && decision.settlement == RecoveryContinuing
	case RecoveryInstallPublishing:
		return decision.nextPhase == FilePublishing && decision.settlement == RecoveryContinuing
	case RecoveryInstallPublished:
		return decision.nextPhase == FilePublished && decision.settlement == RecoveryContinuing
	case RecoveryInstallPublishBlocked:
		return decision.nextPhase == FilePublishBlocked && decision.settlement == RecoveryPublishBlocked
	case RecoveryInstallRetiring:
		return decision.nextPhase == FileRetiring && decision.retirementReason.Valid() &&
			decision.settlement == RecoveryContinuing
	case RecoveryInstallQuarantine:
		return decision.nextPhase == FileQuarantined && decision.quarantineReason.Valid() &&
			decision.settlement == RecoveryNeedsAttention
	}
	if decision.nextPhase != 0 {
		return false
	}
	switch decision.action {
	case RecoveryRetryObjectCreation, RecoveryRequireRevisionBinding, RecoveryLinkFinalNoReplace,
		RecoverySyncFinalParent:
		return decision.settlement == RecoveryContinuing
	case RecoveryResumeContent:
		return decision.settlement == RecoveryReadyForContent
	case RecoveryHoldPublishBlocked:
		return decision.settlement == RecoveryPublishBlocked
	case RecoveryRemovePublishedStageAndSync, RecoverySyncPublishedStageParent:
		return decision.settlement == RecoveryPublished
	case RecoveryHoldPublishedCleanup:
		return record.phase == FilePublished && decision.settlement == RecoveryNeedsAttention
	case RecoveryRemoveRetiringStageAndSync, RecoverySyncStageRemoveAnchorAndSync:
		return decision.settlement == RecoveryRetiring
	case RecoverySyncParentsRemoveRecordAndSync:
		return decision.settlement == RecoveryRetired ||
			decision.settlement == RecoveryCollision && record.retirementReason == RetirementPreObjectCollision
	case RecoveryHoldRetiringCleanup:
		return record.phase == FileRetiring && decision.settlement == RecoveryNeedsAttention
	case RecoveryHoldQuarantine:
		return decision.settlement == RecoveryNeedsAttention
	default:
		return false
	}
}
