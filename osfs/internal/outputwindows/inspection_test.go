//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

type windowsObjectInspectorFunc func(windows.Handle) (windowsObjectFacts, error)

func (inspect windowsObjectInspectorFunc) Inspect(handle windows.Handle) (windowsObjectFacts, error) {
	return inspect(handle)
}

func (inspect windowsObjectInspectorFunc) Identify(handle windows.Handle) (windowsObjectIdentity, error) {
	facts, err := inspect(handle)
	return facts.object, err
}

type windowsVolumeInspectorFunc func(windows.Handle) (windowsVolumeFacts, error)

func (inspect windowsVolumeInspectorFunc) InspectVolume(handle windows.Handle) (windowsVolumeFacts, error) {
	return inspect(handle)
}

func TestWindowsVolumeIdentitySchemeIsScopedToOpenRoot(t *testing.T) {
	path := windowsNativeTestTempDir(t)
	volumeInspections := 0
	volumes := windowsVolumeInspectorFunc(func(handle windows.Handle) (windowsVolumeFacts, error) {
		volumeInspections++
		return (nativeWindowsVolumeInspector{}).InspectVolume(handle)
	})
	platform, err := openWindowsOutputPlatformWithInspectors(path, nativeWindowsObjectInspector{}, volumes)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if volumeInspections != 1 {
		t.Fatalf("root admission inspected the volume %d times", volumeInspections)
	}

	// Probe, ancestry, publication and cleanup all exercise the real native
	// object inspector. Their work must not repeat root volume identityScheme.
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if _, err := guard.Root().destinationCapabilities(); err != nil {
		t.Fatal(err)
	}
	directory, err := guard.Root().CreatePrivateDirectory("inspection")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	stage, err := directory.CreatePrivateFile("stage")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	anchor, err := directory.LinkRegularFileNoReplace(stage, "anchor")
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	other, err := directory.CreatePrivateFile("other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	const repeatedChecks = 3
	for range repeatedChecks {
		if same, err := sameWindowsOpenedObject(stage, anchor); err != nil || !same {
			t.Fatalf("hard-link identity = %t, %v", same, err)
		}
		if same, err := sameWindowsOpenedObject(stage, other); err != nil || same {
			t.Fatalf("distinct file identity = %t, %v", same, err)
		}
		if same, err := sameWindowsOpenedDirectory(platform.Root(), guard.Root()); err != nil || !same {
			t.Fatalf("guarded directory identity = %t, %v", same, err)
		}
		if err := errors.Join(stage.verify(true), directory.Sync(), guard.Root().Sync()); err != nil {
			t.Fatal(err)
		}
	}
	if err := directory.RemoveRegularLink("anchor", anchor); err != nil {
		t.Fatal(err)
	}
	if volumeInspections != 1 {
		t.Fatalf("ordinary output work repeated volume identityScheme: %d inspections", volumeInspections)
	}
	if err := errors.Join(other.Close(), anchor.Close(), stage.Close(), directory.Close(), guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
	reopened, err := openWindowsOutputPlatformWithInspectors(path, nativeWindowsObjectInspector{}, volumes)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if volumeInspections != 2 {
		t.Fatalf("reopened root reused stale identityScheme: %d inspections", volumeInspections)
	}
}

func TestWindowsVolumeAdmissionFailureDoesNotCreateOutputState(t *testing.T) {
	injected := errors.New("volume inspection unavailable")
	for _, test := range []struct {
		name    string
		volumes windowsVolumeInspector
	}{
		{name: "missing volume inspector"},
		{name: "volume inspection failure", volumes: windowsVolumeInspectorFunc(
			func(windows.Handle) (windowsVolumeFacts, error) {
				return windowsVolumeFacts{}, injected
			},
		)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := windowsNativeTestTempDir(t)
			platform, err := openWindowsOutputPlatformWithInspectors(path, nativeWindowsObjectInspector{}, test.volumes)
			if platform != nil {
				_ = platform.Close()
				t.Fatal("failed volume admission returned a platform")
			}
			if !errors.Is(err, errWindowsOutputUnsupported) {
				t.Fatalf("volume admission = %v", err)
			}
			if test.volumes != nil && !errors.Is(err, injected) {
				t.Fatalf("volume failure lost its cause: %v", err)
			}
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed volume admission mutated root: entries=%v error=%v", entries, err)
			}
		})
	}
}

func TestWindowsObjectInspectionReadsLiveHandleFacts(t *testing.T) {
	platform := openWindowsTestPlatform(t)
	defer platform.Close()
	file, err := platform.Root().CreatePrivateFile("live-facts")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	inspector := nativeWindowsObjectInspector{}
	before, err := inspector.Inspect(file.handle())
	if err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(platform.Root().path, "live-facts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(path, before.attributes|windows.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	after, err := inspector.Inspect(file.handle())
	if err != nil {
		t.Fatal(err)
	}
	if !before.object.same(after.object) || after.attributes&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Fatalf("live object facts lost identity or cached attributes: before=%+v after=%+v", before, after)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.Inspect(file.handle()); err == nil {
		t.Fatal("closed handle retained cached object facts")
	}
	if _, err := (nativeWindowsVolumeInspector{}).InspectVolume(windows.InvalidHandle); err == nil {
		t.Fatal("invalid root handle was certified")
	}
}
