//go:build darwin

package outputdarwin

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// RootBinding is versioned identity evidence, never permission to reopen a path.
func (platform *Platform) RootBinding() (outputcap.OutputRootBinding, error) {
	if platform == nil || platform.root == nil {
		return outputcap.OutputRootBinding{}, outputcap.ErrUnsafeNamespace
	}
	claim, err := platform.root.PersistentDirectoryIdentityClaim()
	if err != nil {
		return outputcap.OutputRootBinding{}, err
	}
	return outputcap.NewOutputRootBinding(platform.IdentityScheme(), platform.root.volume.uuid[:], claim)
}
func (platform *Platform) ProbeRecoverableFeatures() error {
	capabilities, err := platform.DestinationCapabilities()
	if err != nil {
		return err
	}
	mode, err := outputcap.SelectExecutionMode(capabilities)
	if err != nil || mode != outputcap.ExecutionResumable {
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	}
	return nil
}
