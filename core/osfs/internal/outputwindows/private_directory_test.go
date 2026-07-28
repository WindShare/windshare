//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	windowsPrivateCreateCrashChildEnvironment  = "WINDSHARE_WINDOWS_PRIVATE_CREATE_CHILD"
	windowsPrivateCreateCrashCutEnvironment    = "WINDSHARE_WINDOWS_PRIVATE_CREATE_CUT"
	windowsPrivateCreateCrashTargetEnvironment = "WINDSHARE_WINDOWS_PRIVATE_CREATE_TARGET"
)

var errWindowsPrivateCreateCutInjected = errors.New("injected Windows private-directory create cut")

func TestWindowsV3PrivateDirectoryCreateCutsLeaveOnlyClassifiableState(t *testing.T) {
	cuts := windowsPrivateDirectoryCreateCuts()
	for _, cut := range cuts {
		t.Run(cut.String(), func(t *testing.T) {
			platform := openWindowsV3TestPlatform(t)
			defer platform.Close()
			target := "private-" + cut.String()
			platform.root.createObserver = windowsV3PrivateDirectoryCreateObserverFunc(
				func(_, observedTarget string, observedCut windowsV3PrivateDirectoryCreateCut) error {
					if observedTarget == target && observedCut == cut {
						return errWindowsPrivateCreateCutInjected
					}
					return nil
				},
			)
			created, err := platform.root.CreatePrivateDirectory(target)
			if created != nil {
				_ = created.Close()
			}
			if !errors.Is(err, errWindowsPrivateCreateCutInjected) {
				t.Fatalf("create at %s cut error = %v", cut, err)
			}
			platform.root.createObserver = nil
			assertWindowsPrivateCreateCutState(t, platform.root, target, windowsPrivateCreateCutCommitted(cut))
		})
	}
}

func TestWindowsV3PrivateDirectoryCreateProtocolPropagatesToNestedStateParents(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	const target = "aa"
	platform.root.createObserver = windowsV3PrivateDirectoryCreateObserverFunc(
		func(parent, observedTarget string, cut windowsV3PrivateDirectoryCreateCut) error {
			if filepath.Base(parent) == "session-shell" && observedTarget == target && cut == windowsV3PrivateDirectoryCutCreated {
				return errWindowsPrivateCreateCutInjected
			}
			return nil
		},
	)
	session, err := platform.root.CreatePrivateDirectory("session-shell")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if created, err := session.CreatePrivateDirectory(target); !errors.Is(err, errWindowsPrivateCreateCutInjected) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("nested create cut error = %v", err)
	}
	if kind, err := session.observeEntry(target); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("nested precommit cut left kind=%d error=%v", kind, err)
	}
}

func TestWindowsV3PrivateDirectoryCreateNeverDeletesExistingTarget(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	const target = "existing-private"
	existing, err := platform.root.CreatePrivateDirectory(target)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	wantID, wantPrepared, wantErr := existing.cachedPersistentObjectID()
	if wantErr != nil || !wantPrepared {
		t.Fatal("existing private target lacks a prepared Object ID")
	}
	if created, err := platform.root.CreatePrivateDirectory(target); !errors.Is(err, errWindowsV3OutputCollision) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("second exclusive create error = %v", err)
	}
	reopened, err := platform.root.OpenPrivateDirectory(target)
	if err != nil {
		t.Fatalf("existing target was removed: %v", err)
	}
	defer reopened.Close()
	same, compareErr := sameWindowsV3OpenedDirectory(existing, reopened)
	reopenedID, reopenedPrepared, reopenedIDErr := reopened.cachedPersistentObjectID()
	if compareErr != nil || reopenedIDErr != nil || !same || !reopenedPrepared || reopenedID != wantID {
		t.Fatalf("existing target changed: same=%v id=%x prepared=%t want=%x error=%v",
			same, reopenedID, reopenedPrepared, wantID, errors.Join(compareErr, reopenedIDErr))
	}
}

func TestWindowsNTFSPrivateDirectoryCreateSurvivesRealProcessKillAtEveryCut(t *testing.T) {
	if os.Getenv(windowsPrivateCreateCrashChildEnvironment) == "1" {
		runWindowsPrivateCreateCrashChild(t)
		return
	}
	requireUnprivilegedWindowsNTFSCertification(t)
	for _, cut := range windowsPrivateDirectoryCreateCuts() {
		t.Run(cut.String(), func(t *testing.T) {
			base := t.TempDir()
			rootPath := filepath.Join(base, "output")
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatal(err)
			}
			target := "killed-" + cut.String()
			readyPath := filepath.Join(base, "child.ready")
			killNativeOutputChildAfterReady(t, readyPath, []string{
				windowsPrivateCreateCrashChildEnvironment + "=1",
				windowsPrivateCreateCrashCutEnvironment + "=" + cut.String(),
				windowsPrivateCreateCrashTargetEnvironment + "=" + target,
				nativeOutputCrashRootEnvironment + "=" + rootPath,
				nativeOutputCrashReadyEnvironment + "=" + readyPath,
			})
			platform, err := openWindowsV3OutputPlatform(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			assertWindowsPrivateCreateCutState(t, platform.root, target, windowsPrivateCreateCutCommitted(cut))
		})
	}
}

func runWindowsPrivateCreateCrashChild(t *testing.T) {
	rootPath := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	target := os.Getenv(windowsPrivateCreateCrashTargetEnvironment)
	cut, err := parseWindowsPrivateDirectoryCreateCut(os.Getenv(windowsPrivateCreateCrashCutEnvironment))
	if err != nil || rootPath == "" || readyPath == "" || target == "" {
		t.Fatalf("invalid private-create crash child parameters: %v", err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	platform.root.createObserver = windowsV3PrivateDirectoryCreateObserverFunc(
		func(_, observedTarget string, observedCut windowsV3PrivateDirectoryCreateCut) error {
			if observedTarget != target || observedCut != cut {
				return nil
			}
			signalNativeOutputCrashCut(t, readyPath)
			time.Sleep(nativeOutputCrashChildMaximumWait)
			t.Fatal("private-directory create child was not terminated")
			return nil
		},
	)
	if _, err := platform.root.CreatePrivateDirectory(target); err != nil {
		t.Fatal(err)
	}
	t.Fatal("private-directory create child passed its requested kill cut")
}

func assertWindowsPrivateCreateCutState(
	t *testing.T,
	root *windowsV3Directory,
	target string,
	present bool,
) {
	t.Helper()
	kind, err := root.observeEntry(target)
	if !present {
		if err != nil || kind != outputcap.EntryAbsent {
			t.Fatalf("precommit cut left target kind=%d error=%v", kind, err)
		}
		created, err := root.CreatePrivateDirectory(target)
		if err != nil {
			t.Fatalf("precommit cut blocked a later strict create: %v", err)
		}
		if err := created.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil || kind != outputcap.EntryDirectory {
		t.Fatalf("committed cut target kind=%d error=%v", kind, err)
	}
	opened, err := root.OpenPrivateDirectory(target)
	if err != nil {
		t.Fatalf("committed cut is not classifiable: %v", err)
	}
	identity, prepared, identityErr := opened.cachedPersistentObjectID()
	if identityErr != nil || !prepared || !identity.valid() {
		_ = opened.Close()
		t.Fatal("committed cut lacks a persistent NTFS object ID")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func windowsPrivateDirectoryCreateCuts() []windowsV3PrivateDirectoryCreateCut {
	return []windowsV3PrivateDirectoryCreateCut{
		windowsV3PrivateDirectoryCutCreated,
		windowsV3PrivateDirectoryCutObjectID,
		windowsV3PrivateDirectoryCutACLHidden,
		windowsV3PrivateDirectoryCutSynced,
		windowsV3PrivateDirectoryCutCommitted,
		windowsV3PrivateDirectoryCutClosed,
	}
}

func windowsPrivateCreateCutCommitted(cut windowsV3PrivateDirectoryCreateCut) bool {
	return cut == windowsV3PrivateDirectoryCutCommitted || cut == windowsV3PrivateDirectoryCutClosed
}

func parseWindowsPrivateDirectoryCreateCut(raw string) (windowsV3PrivateDirectoryCreateCut, error) {
	for _, cut := range windowsPrivateDirectoryCreateCuts() {
		if cut.String() == raw {
			return cut, nil
		}
	}
	return 0, fmt.Errorf("unknown private-directory create cut %q", raw)
}
