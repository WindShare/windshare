package destinationauthority

import (
	"errors"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Unknown cleanup entries do not revoke a separately authenticated operation
// registry. They remain untouched while normal recovery retains its authority.
func bindCleanup(config BindConfig, control, root outputcap.Directory,
	capabilities outputcap.DestinationCapabilities, profile checkpointmodel.LiveCleanupNativeProfile,
) (journal LiveCleanupJournalHandle, proof outputcap.Directory, result outputcap.DestinationCapabilities, resultErr error) {
	result = capabilities
	if !capabilities.CrashCleanup().Supported() {
		return journal, nil, result, validateNativeMethodSets(root, nil, capabilities)
	}
	journal, resultErr = config.OpenLiveCleanupJournal(control)
	if resultErr != nil || !journal.valid() {
		return journal, nil, result, errors.Join(ErrInvalidConfiguration, resultErr, journal.Close())
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, journal.Close(), closeDirectory(proof))
		}
	}()
	proof, resultErr = openExactPrivateDirectory(control, checkpointmodel.LiveCleanupNamespaceV1)
	if resultErr != nil {
		return
	}
	resultErr = validateNativeMethodSets(root, proof, capabilities)
	if resultErr != nil {
		return
	}
	result, resultErr = reconcileLiveCleanup(journal.journal, proof, capabilities, profile)
	return
}
