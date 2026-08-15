//go:build windows

package destinationauthority

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

func TestWindowsNativeFactoryProvidesOrdinaryDestinationOptionalMethods(t *testing.T) {
	platform, err := outputwindows.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if _, ok := platform.(destinationCapabilitySource); !ok {
		t.Fatal("Windows platform lacks DestinationCapabilities")
	}
	if _, ok := platform.(liveCleanupProfileSource); !ok {
		t.Fatal("Windows platform lacks LiveCleanupNativeProfile")
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	assertWindowsDirectoryOptionalMethods(t, root)
}

func assertWindowsDirectoryOptionalMethods(t *testing.T, directory outputcap.Directory) {
	t.Helper()
	if _, ok := directory.(fileNoReplacePublisher); !ok {
		t.Fatal("Windows directory lacks PublishFileNoReplace")
	}
	if _, ok := directory.(publicDirectoryReserver); !ok {
		t.Fatal("Windows directory lacks ReservePublicDirectoryNoReplace")
	}
	if _, ok := directory.(liveCleanupStageCreator); !ok {
		t.Fatal("Windows directory lacks CreateLiveCleanupStage")
	}
	if _, ok := directory.(liveCleanupStageRemover); !ok {
		t.Fatal("Windows directory lacks RemoveLiveCleanupStage")
	}
	if _, ok := directory.(outputcap.PersistentDirectoryIdentity); !ok {
		t.Fatal("Windows directory lacks persistent identity read")
	}
	if _, ok := directory.(outputcap.PersistentDirectoryIdentityPreparer); !ok {
		t.Fatal("Windows directory lacks persistent identity enrollment")
	}
}
