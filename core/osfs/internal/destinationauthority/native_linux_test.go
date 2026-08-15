//go:build linux

package destinationauthority

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func TestLinuxNativeFactoryProvidesOrdinaryDestinationOptionalMethods(t *testing.T) {
	platform, err := outputlinux.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if _, ok := platform.(destinationCapabilitySource); !ok {
		t.Fatal("Linux platform lacks DestinationCapabilities")
	}
	if _, ok := platform.(liveCleanupProfileSource); !ok {
		t.Fatal("Linux platform lacks LiveCleanupNativeProfile")
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	assertLinuxDirectoryOptionalMethods(t, root)
}

func assertLinuxDirectoryOptionalMethods(t *testing.T, directory outputcap.Directory) {
	t.Helper()
	if _, ok := directory.(fileNoReplacePublisher); !ok {
		t.Fatal("Linux directory lacks PublishFileNoReplace")
	}
	if _, ok := directory.(publicDirectoryReserver); !ok {
		t.Fatal("Linux directory lacks ReservePublicDirectoryNoReplace")
	}
	if _, ok := directory.(liveCleanupStageCreator); !ok {
		t.Fatal("Linux directory lacks CreateLiveCleanupStage")
	}
	if _, ok := directory.(liveCleanupStageRemover); !ok {
		t.Fatal("Linux directory lacks RemoveLiveCleanupStage")
	}
	if _, ok := directory.(outputcap.PersistentDirectoryIdentity); !ok {
		t.Fatal("Linux directory lacks persistent identity read")
	}
	if _, ok := directory.(outputcap.PersistentDirectoryIdentityPreparer); !ok {
		t.Fatal("Linux directory lacks persistent identity enrollment")
	}
}
