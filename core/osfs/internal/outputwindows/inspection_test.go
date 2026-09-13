//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

type windowsV3VolumeInspectorFunc func(windows.Handle) (windowsV3VolumeFacts, error)

func (inspect windowsV3VolumeInspectorFunc) InspectVolume(handle windows.Handle) (windowsV3VolumeFacts, error) {
	return inspect(handle)
}

func TestWindowsV3VolumeCertificationIsScopedToOpenRoot(t *testing.T) {
	path := windowsV3NativeTestTempDir(t)
	volumeInspections := 0
	volumes := windowsV3VolumeInspectorFunc(func(handle windows.Handle) (windowsV3VolumeFacts, error) {
		volumeInspections++
		return (nativeWindowsV3VolumeInspector{}).InspectVolume(handle)
	})
	platform, err := openWindowsV3OutputPlatformWithInspectors(path, nativeWindowsV3ObjectInspector{}, volumes)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if volumeInspections != 1 {
		t.Fatalf("root admission inspected the volume %d times", volumeInspections)
	}

	// Probe, ancestry, publication and cleanup all exercise the real native
	// object inspector. Their work must not repeat root volume certification.
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
		if same, err := sameWindowsV3OpenedObject(stage, anchor); err != nil || !same {
			t.Fatalf("hard-link identity = %t, %v", same, err)
		}
		if same, err := sameWindowsV3OpenedObject(stage, other); err != nil || same {
			t.Fatalf("distinct file identity = %t, %v", same, err)
		}
		if same, err := sameWindowsV3OpenedDirectory(platform.Root(), guard.Root()); err != nil || !same {
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
		t.Fatalf("ordinary output work repeated volume certification: %d inspections", volumeInspections)
	}
	if err := errors.Join(other.Close(), anchor.Close(), stage.Close(), directory.Close(), guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
	reopened, err := openWindowsV3OutputPlatformWithInspectors(path, nativeWindowsV3ObjectInspector{}, volumes)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if volumeInspections != 2 {
		t.Fatalf("reopened root reused stale certification: %d inspections", volumeInspections)
	}
}

func TestWindowsV3VolumeAdmissionFailureDoesNotCreateOutputState(t *testing.T) {
	injected := errors.New("volume inspection unavailable")
	for _, test := range []struct {
		name    string
		volumes windowsV3VolumeInspector
	}{
		{name: "missing volume inspector"},
		{name: "volume inspection failure", volumes: windowsV3VolumeInspectorFunc(
			func(windows.Handle) (windowsV3VolumeFacts, error) {
				return windowsV3VolumeFacts{}, injected
			},
		)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := windowsV3NativeTestTempDir(t)
			platform, err := openWindowsV3OutputPlatformWithInspectors(path, nativeWindowsV3ObjectInspector{}, test.volumes)
			if platform != nil {
				_ = platform.Close()
				t.Fatal("failed volume admission returned a platform")
			}
			if !errors.Is(err, errWindowsV3OutputUnsupported) {
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

func TestWindowsV3ObjectInspectionReadsLiveHandleFacts(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	file, err := platform.Root().CreatePrivateFile("live-facts")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	inspector := nativeWindowsV3ObjectInspector{}
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
	if _, err := (nativeWindowsV3VolumeInspector{}).InspectVolume(windows.InvalidHandle); err == nil {
		t.Fatal("invalid root handle was certified")
	}
}
