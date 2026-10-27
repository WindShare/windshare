package destinationauthority

import (
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
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

// Recovery evidence describes the versioned native identity and flush contract.
// Successful live probes alone cannot establish either restart guarantee.
type recoveryEvidenceSource interface {
	IdentityScheme() outputcap.IdentitySchemeID
	Durability() transfer.DurabilityLevel
}
