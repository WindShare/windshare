package resumestate

import "fmt"

type AnchorObservation uint8

const (
	AnchorMissing AnchorObservation = iota + 1
	AnchorVerified
	AnchorUnsafe
)

type EntryObservation uint8

const (
	EntryNotObserved EntryObservation = iota
	EntryMissing
	EntrySameAsAnchor
	EntryDifferentFromAnchor
	// EntryPresentUnresolved is safe presence information when no verified anchor
	// exists. It never grants permission to mutate that entry.
	EntryPresentUnresolved
	EntryUnsafe
)

type MetadataObservation uint8

const (
	MetadataNotObserved MetadataObservation = iota
	MetadataMatches
	MetadataDiffers
	MetadataUnsafe
)

type FinalParentObservation uint8

const (
	FinalParentNotObserved FinalParentObservation = iota
	FinalParentSyncRequired
	FinalParentSynced
)

type FileObservation struct {
	Anchor      AnchorObservation
	Stage       EntryObservation
	Final       EntryObservation
	Metadata    MetadataObservation
	FinalParent FinalParentObservation
}

// InternalFileObservationRequiresQuarantine reports when anchor/stage evidence
// is already conclusive, so a later failure to reopen the public parent cannot
// erase stronger evidence by turning recovery into a retryable operation error.
func InternalFileObservationRequiresQuarantine(
	phase FilePhase,
	observation FileObservation,
) bool {
	_, conclusive := internalFileObservationQuarantine(phase, observation)
	return conclusive
}

func internalFileObservationQuarantine(
	phase FilePhase,
	observation FileObservation,
) (QuarantineReason, bool) {
	if !phase.Valid() || observation.Anchor < AnchorMissing || observation.Anchor > AnchorUnsafe {
		return 0, false
	}
	// Published and retiring records use internal entries only as cleanup
	// witnesses. Partial cleanup evidence cannot authorize a phase change; a
	// complete observation must either prove the next ordered cut or hold state.
	if phase == FilePublished || phase == FileRetiring {
		return 0, false
	}
	if observation.Stage == EntryNotObserved {
		return quarantineForUnobservedStage(phase, observation.Anchor)
	}
	if observation.Stage < EntryMissing || observation.Stage > EntryUnsafe ||
		!entryObservationHasAnchorAuthority(observation.Anchor, observation.Stage) {
		return 0, false
	}
	if observation.Anchor == AnchorUnsafe {
		return QuarantineAnchorUnsafe, true
	}
	if observation.Stage == EntryUnsafe {
		return QuarantineStageUnsafe, true
	}
	if observation.Anchor == AnchorMissing {
		return quarantineForMissingAnchor(phase, observation.Stage)
	}
	return quarantineForVerifiedAnchor(phase, observation.Stage)
}

func quarantineForUnobservedStage(
	phase FilePhase,
	anchor AnchorObservation,
) (QuarantineReason, bool) {
	if anchor == AnchorUnsafe {
		return QuarantineAnchorUnsafe, true
	}
	if anchor == AnchorMissing && phase != FileReserved {
		return QuarantineAnchorMissing, true
	}
	return 0, false
}

func entryObservationHasAnchorAuthority(anchor AnchorObservation, entry EntryObservation) bool {
	if anchor == AnchorVerified {
		return entry != EntryPresentUnresolved
	}
	return entry != EntrySameAsAnchor && entry != EntryDifferentFromAnchor
}

func quarantineForMissingAnchor(
	phase FilePhase,
	stage EntryObservation,
) (QuarantineReason, bool) {
	if phase != FileReserved {
		return QuarantineAnchorMissing, true
	}
	if stage == EntryPresentUnresolved {
		return QuarantinePartialObjectCreation, true
	}
	return 0, false
}

func quarantineForVerifiedAnchor(
	phase FilePhase,
	stage EntryObservation,
) (QuarantineReason, bool) {
	if stage == EntryDifferentFromAnchor {
		return QuarantineStageMismatch, true
	}
	if stage == EntryMissing {
		if phase == FileReserved {
			return QuarantinePartialObjectCreation, true
		}
		return QuarantineStageMissing, true
	}
	return 0, false
}

func validateObservation(phase FilePhase, observation FileObservation) error {
	if observation.Anchor < AnchorMissing || observation.Anchor > AnchorUnsafe {
		return fmt.Errorf("%w: anchor observation", ErrInvalidState)
	}
	if phase == FileRetiring {
		if observation.Final != EntryNotObserved || observation.Metadata != MetadataNotObserved ||
			observation.FinalParent != FinalParentNotObserved {
			return fmt.Errorf("%w: retiring observes final state", ErrInvalidState)
		}
		return validateEntryRelations(observation.Anchor, observation.Stage, EntryNotObserved)
	}
	if observation.Final == EntryNotObserved || observation.Stage == EntryNotObserved {
		return fmt.Errorf("%w: incomplete file observation", ErrInvalidState)
	}
	if observation.Final != EntrySameAsAnchor && observation.Metadata != MetadataNotObserved {
		return fmt.Errorf("%w: metadata without matching final", ErrInvalidState)
	}
	if observation.Final != EntrySameAsAnchor && observation.FinalParent != FinalParentNotObserved ||
		observation.FinalParent > FinalParentSynced {
		return fmt.Errorf("%w: final parent sync without matching final", ErrInvalidState)
	}
	return validateEntryRelations(observation.Anchor, observation.Stage, observation.Final)
}

func validateEntryRelations(anchor AnchorObservation, stage, final EntryObservation) error {
	entries := []EntryObservation{stage}
	if final != EntryNotObserved {
		entries = append(entries, final)
	}
	for _, entry := range entries {
		if entry < EntryMissing || entry > EntryUnsafe {
			return fmt.Errorf("%w: entry observation", ErrInvalidState)
		}
		if anchor == AnchorVerified && entry == EntryPresentUnresolved ||
			anchor != AnchorVerified && (entry == EntrySameAsAnchor || entry == EntryDifferentFromAnchor) {
			return fmt.Errorf("%w: entry relation without anchor authority", ErrInvalidState)
		}
	}
	return nil
}
