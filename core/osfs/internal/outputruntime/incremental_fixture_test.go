package outputruntime

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointcleaner"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
)

type runtimeTestRootSpec struct {
	path   string
	create bool
}

func newIncrementalTestAuthority(
	t *testing.T,
	root string,
	platformFactory PlatformFactory,
) *Authority {
	t.Helper()
	if platformFactory == nil {
		platformFactory = openOutputRuntimeTestPlatform
	}
	authority, err := New(Config{
		RootPath: root, PlatformFactory: platformFactory,
		CheckpointCleanup: func(
			_ context.Context,
			config checkpointcleaner.OneShotCheckpointCleanerConfig,
		) (checkpointcleaner.CheckpointCleanupReport, error) {
			binding, err := config.Platform.RootBinding()
			if err != nil {
				return checkpointcleaner.CheckpointCleanupReport{}, err
			}
			if err := checkpointstore.BootstrapOwnership(checkpointstore.NamespaceConfig{
				Root: config.Platform.Root(), BackendID: config.BackendID,
				RootIdentity: binding.Bytes(),
			}); err != nil {
				return checkpointcleaner.CheckpointCleanupReport{}, err
			}
			return checkpointcleaner.CheckpointCleanupReport{
				Status: checkpointcleaner.CheckpointCleanupStatusComplete, Complete: true,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func incrementalTestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}
