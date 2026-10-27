package destinationauthority

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

var ErrCleanupOwnershipUnknown = errors.New("cleanup ownership is unknown")

// Unknown cleanup entries do not revoke a separately authenticated operation
// registry. They remain untouched while normal recovery retains its authority.
func bindCleanup(config BindConfig, control, root outputcap.Directory,
	capabilities outputcap.DestinationCapabilities, profile checkpointmodel.LiveCleanupNativeProfile,
) (journal LiveCleanupJournalHandle, proof outputcap.Directory, result outputcap.DestinationCapabilities, resultErr error) {
	result = capabilities
	if !capabilities.CrashCleanup().Supported() {
		return journal, nil, result, validateNativeMethodSets(root, nil, capabilities)
	}
	defer func() {
		uncertain := errors.Is(resultErr, ErrCleanupOwnershipUnknown) || errors.Is(resultErr, outputcap.ErrUnsafeNamespace)
		if uncertain {
			result, resultErr = downgradedCleanupCapabilities(capabilities, outputcap.CapabilityReasonCleanupOwnershipUnknown)
		}
		if uncertain || resultErr != nil {
			resultErr = errors.Join(resultErr, journal.Close(), closeDirectory(proof))
			proof = nil
		}
	}()
	journal, resultErr = config.OpenLiveCleanupJournal(control)
	if resultErr != nil {
		return
	}
	if !journal.valid() {
		resultErr = ErrInvalidConfiguration
		return
	}
	proof, resultErr = openCleanupProof(control)
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

// Unlike a registry failure, uncertainty in this sibling namespace only removes
// cleanup authority. Preserve I/O errors so a retry can establish evidence again.
func openCleanupProof(control outputcap.Directory) (outputcap.Directory, error) {
	kind, exact, err := control.ClassifyExactEntry(checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil {
		return nil, err
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, ErrCleanupOwnershipUnknown
	}
	reference, err := control.OpenEntry(checkpointmodel.LiveCleanupNamespaceV1)
	if errors.Is(err, fs.ErrNotExist) {
		err = ErrCleanupOwnershipUnknown
	}
	if err != nil {
		return nil, errors.Join(err, closeEntry(reference))
	}
	if reference == nil || reference.Kind() != outputcap.EntryDirectory {
		return nil, errors.Join(ErrCleanupOwnershipUnknown, closeEntry(reference))
	}
	opened, openErr := control.OpenPinnedDirectory(reference, true)
	closeErr := reference.Close()
	if openErr != nil || closeErr != nil || opened == nil {
		if openErr == nil && opened == nil {
			openErr = ErrCleanupOwnershipUnknown
		}
		return nil, errors.Join(openErr, closeErr, closeDirectory(opened))
	}
	return opened, nil
}
