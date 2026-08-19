package outputruntime

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

type ordinaryResumeDecision struct {
	state  resumeauthority.ItemState
	reason resumeauthority.ItemBlockReason
}

func ordinaryResumeState(state resumeauthority.ItemState) ordinaryResumeDecision {
	return ordinaryResumeDecision{state: state, reason: resumeauthority.ItemBlockNone}
}

func ordinaryResumeBlock(reason resumeauthority.ItemBlockReason) ordinaryResumeDecision {
	return ordinaryResumeDecision{state: resumeauthority.ItemBlocked, reason: reason}
}

func (decision ordinaryResumeDecision) item(
	record checkpointmodel.Record,
) (resumeauthority.Item, error) {
	return resumeauthority.NewItem(record.CanonicalPath(), decision.state, decision.reason)
}

func reduceOrdinaryResumeRecord(
	record checkpointmodel.Record,
	owned fileexecution.OwnedObservation,
	final fileexecution.FinalObservation,
) ordinaryResumeDecision {
	if record.Phase() == checkpointmodel.PhaseQuarantined ||
		record.CommitState() == checkpointmodel.CommitQuarantined {
		return ordinaryResumeBlock(resumeauthority.ItemBlockCheckpointInvalid)
	}
	switch record.Phase() {
	case checkpointmodel.PhasePublished:
		return reducePublishedResumeRecord(final)
	case checkpointmodel.PhasePublishing:
		return reducePublishingResumeRecord(owned, final)
	case checkpointmodel.PhaseReserved, checkpointmodel.PhaseActive, checkpointmodel.PhasePaused:
		return reduceWritableResumeRecord(record, owned, final)
	case checkpointmodel.PhaseRetired:
		return reduceRetiredResumeRecord(record, final)
	default:
		return ordinaryResumeBlock(resumeauthority.ItemBlockCheckpointInvalid)
	}
}

func reducePublishedResumeRecord(
	final fileexecution.FinalObservation,
) ordinaryResumeDecision {
	if final.Condition() == fileexecution.FinalOwnedExact {
		return ordinaryResumeState(resumeauthority.ItemPublished)
	}
	return ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown)
}

func reducePublishingResumeRecord(
	owned fileexecution.OwnedObservation,
	final fileexecution.FinalObservation,
) ordinaryResumeDecision {
	switch final.Condition() {
	case fileexecution.FinalOwnedExact:
		return ordinaryResumeState(resumeauthority.ItemPublished)
	case fileexecution.FinalAbsent, fileexecution.FinalCollision:
		// Destination occupation changes only when publication may proceed. It
		// cannot erase the retry authority of an intact private object.
		if owned.Condition() == fileexecution.OwnedReady {
			return ordinaryResumeState(resumeauthority.ItemResumable)
		}
		return ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown)
	default:
		return ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown)
	}
}

func reduceWritableResumeRecord(
	record checkpointmodel.Record,
	owned fileexecution.OwnedObservation,
	final fileexecution.FinalObservation,
) ordinaryResumeDecision {
	switch final.Condition() {
	case fileexecution.FinalAbsent:
		return reduceAbsentWritableResumeRecord(record, owned)
	case fileexecution.FinalCollision:
		// Authenticated lineage prevents an occupied final from becoming a new
		// collision claim; only the private object's authority controls retry.
		if owned.Condition() == fileexecution.OwnedReady {
			return writableResumeState(record)
		}
		return ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown)
	default:
		return ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown)
	}
}

func reduceAbsentWritableResumeRecord(
	record checkpointmodel.Record,
	owned fileexecution.OwnedObservation,
) ordinaryResumeDecision {
	switch owned.Condition() {
	case fileexecution.OwnedReady:
		return writableResumeState(record)
	case fileexecution.OwnedAbsent:
		if checkpointmodel.InitialCandidate(record) {
			// Only the pre-object crash cut may recreate its already recorded ID.
			return ordinaryResumeState(resumeauthority.ItemIncomplete)
		}
		return ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown)
	default:
		return ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown)
	}
}

func writableResumeState(record checkpointmodel.Record) ordinaryResumeDecision {
	if len(record.VerifiedRanges()) != 0 || record.Phase() == checkpointmodel.PhasePaused {
		return ordinaryResumeState(resumeauthority.ItemResumable)
	}
	return ordinaryResumeState(resumeauthority.ItemIncomplete)
}

func reduceRetiredResumeRecord(
	record checkpointmodel.Record,
	final fileexecution.FinalObservation,
) ordinaryResumeDecision {
	if record.RetirementReason() == checkpointmodel.RetirementPublished &&
		final.Condition() == fileexecution.FinalOwnedExact {
		return ordinaryResumeState(resumeauthority.ItemPublished)
	}
	return ordinaryResumeState(resumeauthority.ItemFailed)
}
