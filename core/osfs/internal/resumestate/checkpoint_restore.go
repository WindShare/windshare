package resumestate

import (
	"fmt"
)

func restoredFilePhase(checkpoint FileCheckpointV1) (FilePhase, error) {
	switch checkpoint.phase {
	case FileCheckpointPhaseActive:
		if checkpoint.commitState == FileCheckpointCommitVerified {
			return FileWitnessed, nil
		}
	case FileCheckpointPhasePublishing:
		if checkpoint.commitState == FileCheckpointCommitVerified {
			return FilePublishing, nil
		}
	case FileCheckpointPhasePaused:
		if checkpoint.commitState == FileCheckpointCommitVerified {
			return FilePublishBlocked, nil
		}
	case FileCheckpointPhasePublished:
		if checkpoint.commitState == FileCheckpointCommitPublished {
			return FilePublished, nil
		}
	case FileCheckpointPhaseRetired:
		if checkpoint.commitState == FileCheckpointCommitVerified {
			return FileRetiring, nil
		}
	case FileCheckpointPhaseQuarantined:
		if checkpoint.commitState == FileCheckpointCommitQuarantined {
			return FileQuarantined, nil
		}
	}
	return 0, fmt.Errorf("%w: unsupported checkpoint lifecycle", ErrFileCheckpointRecovery)
}
