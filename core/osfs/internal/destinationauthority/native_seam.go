package destinationauthority

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type destinationCapabilitySource interface {
	DestinationCapabilities() (outputcap.DestinationCapabilities, error)
}

type liveCleanupProfileSource interface {
	LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile
}

type fileNoReplacePublisher interface {
	PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
}

type publicDirectoryReserver interface {
	ReservePublicDirectoryNoReplace(string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error)
}

type liveCleanupStageCreator interface {
	CreateLiveCleanupStage(outputcap.Directory, checkpointmodel.LiveCleanupTicket) error
}

type liveCleanupStageRemover interface {
	RemoveLiveCleanupStage(checkpointmodel.LiveCleanupTicket, outputcap.FileIdentity) error
}
