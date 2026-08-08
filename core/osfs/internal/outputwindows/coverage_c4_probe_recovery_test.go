//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	coverageC4CompleteProbeName  = ".windshare-output.probe-11111111111111111111111111111111"
	coverageC4ForeignProbeName   = ".windshare-output.probe-22222222222222222222222222222222"
	coverageC4MalformedProbeName = ".windshare-output.probe-not-a-canonical-nonce"
)

func TestCoverageC4WindowsProbeRecoversExactRestartLeftover(t *testing.T) {
	opened, err := Open(windowsV3NativeTestTempDir(t), false)
	if err != nil {
		t.Fatal(err)
	}
	platform, ok := opened.(*windowsOutputV3Platform)
	if !ok {
		_ = opened.Close()
		t.Fatalf("Windows platform type = %T", opened)
	}
	defer platform.Close()
	guard, err := platform.native.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	root := guard.Root()
	coverageC4BuildCompleteProbeLeftover(t, root, coverageC4CompleteProbeName)

	// Injected entropy fixes the fresh probe name so this test covers recovery,
	// the mutex lifetime, and the normal feature proof without timing or chance.
	nonce := bytes.Repeat([]byte{0x5c}, windowsV3OutputProbeRandomBytes)
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(nonce)); err != nil {
		t.Fatal(err)
	}
	if err := root.probeRecoverableFeaturesWithRandom(nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing probe entropy error = %v", err)
	}
	var closedRoot *windowsV3Directory
	if err := closedRoot.probeRecoverableFeaturesWithRandom(bytes.NewReader(nonce)); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed probe root error = %v", err)
	}
	if err := closedRoot.recoverOutputProbeLeftovers(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed recovery root error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.path, coverageC4CompleteProbeName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canonical probe leftover remains: %v", err)
	}
	entries, err := os.ReadDir(root.path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe reconciliation left entries=%v error=%v", entries, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		t.Fatalf("platform feature probe: %v", err)
	}
	if err := platform.native.root.releaseOutputProbeLock(nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing probe lock release error = %v", err)
	}
	moved := &windowsV3OutputProbeLock{handle: windows.Handle(1), held: true}
	if err := platform.native.root.releaseOutputProbeLock(moved); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("unowned-thread probe lock release error = %v", err)
	}
}

func TestCoverageC4WindowsProbeRecoveryPreservesWrongKindRoot(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	name := ".windshare-output.probe-66666666666666666666666666666666"
	foreign, err := root.CreatePrivateFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.recoverOutputProbeLeftovers(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("wrong-kind probe-root error = %v", err)
	}
	if info, err := os.Stat(filepath.Join(root.path, name)); err != nil || info.IsDir() {
		t.Fatalf("wrong-kind probe root was changed: info=%v error=%v", info, err)
	}
}

func TestCoverageC4WindowsProbeRecoveryPreservesUnownedState(t *testing.T) {
	invalidCharacterName := windowsV3OutputProbePrefix + "gggggggggggggggggggggggggggggggg"
	if windowsV3CanonicalProbeName(invalidCharacterName) {
		t.Fatal("probe name with non-hex nonce was canonical")
	}
	for _, test := range []struct {
		name      string
		probeName string
		build     func(*testing.T, *windowsV3Directory)
	}{
		{
			name:      "malformed reserved name",
			probeName: coverageC4MalformedProbeName,
		},
		{
			name:      "foreign child in canonical namespace",
			probeName: coverageC4ForeignProbeName,
			build: func(t *testing.T, probe *windowsV3Directory) {
				file, err := probe.CreatePrivateFile("foreign")
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := openWindowsV3TestPlatform(t)
			defer platform.Close()
			guard, err := platform.acquirePublicOperationGuard()
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			root := guard.Root()
			probe, err := root.CreatePrivateDirectory(test.probeName)
			if err != nil {
				t.Fatal(err)
			}
			if test.build != nil {
				test.build(t, probe)
			}
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}

			if err := root.recoverOutputProbeLeftovers(); !errors.Is(err, errWindowsV3OutputUnsafe) {
				t.Fatalf("unsafe leftover error = %v", err)
			}
			if info, err := os.Stat(filepath.Join(root.path, test.probeName)); err != nil || !info.IsDir() {
				t.Fatalf("unowned leftover was changed: info=%v error=%v", info, err)
			}
		})
	}
}

func TestCoverageC4WindowsProbeRemovalStopsAtIdentityReplacement(t *testing.T) {
	for _, test := range []struct {
		name      string
		probeName string
		build     func(*testing.T, *windowsV3Directory, string)
		replace   func(*testing.T, *windowsV3OutputProbeLeftover)
		preserved string
	}{
		{
			name:      "regular link",
			probeName: ".windshare-output.probe-33333333333333333333333333333333",
			build:     coverageC4BuildCompleteProbeLeftover,
			replace: func(t *testing.T, leftover *windowsV3OutputProbeLeftover) {
				if err := leftover.directory.RemoveRegularLink("stage", leftover.regular["stage"]); err != nil {
					t.Fatal(err)
				}
				foreign, err := leftover.directory.CreatePrivateFile("stage")
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(foreign.Truncate(1), foreign.Close(), leftover.directory.Sync()); err != nil {
					t.Fatal(err)
				}
			},
			preserved: "stage",
		},
		{
			name:      "child directory",
			probeName: ".windshare-output.probe-44444444444444444444444444444444",
			build: func(t *testing.T, root *windowsV3Directory, name string) {
				probe, err := root.CreatePrivateDirectory(name)
				if err != nil {
					t.Fatal(err)
				}
				candidate, err := probe.CreatePrivateDirectory("candidate")
				if err != nil {
					_ = probe.Close()
					t.Fatal(err)
				}
				if err := errors.Join(candidate.Close(), probe.Close()); err != nil {
					t.Fatal(err)
				}
			},
			replace: func(t *testing.T, leftover *windowsV3OutputProbeLeftover) {
				if err := leftover.directory.RemoveDirectory("candidate", leftover.directories["candidate"]); err != nil {
					t.Fatal(err)
				}
				foreign, err := leftover.directory.CreatePrivateDirectory("candidate")
				if err != nil {
					t.Fatal(err)
				}
				if err := foreign.Close(); err != nil {
					t.Fatal(err)
				}
			},
			preserved: "candidate",
		},
		{
			name:      "probe root",
			probeName: ".windshare-output.probe-55555555555555555555555555555555",
			build: func(t *testing.T, root *windowsV3Directory, name string) {
				probe, err := root.CreatePrivateDirectory(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := probe.Close(); err != nil {
					t.Fatal(err)
				}
			},
			replace: func(t *testing.T, leftover *windowsV3OutputProbeLeftover) {
				if err := leftover.root.RemoveDirectory(leftover.name, leftover.directory); err != nil {
					t.Fatal(err)
				}
				foreign, err := leftover.root.CreatePrivateDirectory(leftover.name)
				if err != nil {
					t.Fatal(err)
				}
				if err := foreign.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := openWindowsV3TestPlatform(t)
			defer platform.Close()
			guard, err := platform.acquirePublicOperationGuard()
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			root := guard.Root()
			test.build(t, root, test.probeName)
			leftover, err := root.inspectOutputProbeLeftover(test.probeName)
			if err != nil {
				t.Fatal(err)
			}
			test.replace(t, leftover)
			if err := leftover.remove(); !errors.Is(err, errWindowsV3OutputUnsafe) {
				_ = leftover.close()
				t.Fatalf("replacement removal error = %v", err)
			}
			if err := leftover.close(); err != nil {
				t.Fatal(err)
			}
			preservedPath := filepath.Join(root.path, test.probeName, test.preserved)
			if test.preserved == "" {
				preservedPath = filepath.Join(root.path, test.probeName)
			}
			if _, err := os.Stat(preservedPath); err != nil {
				t.Fatalf("replacement was not preserved: %v", err)
			}
		})
	}
}

func coverageC4BuildCompleteProbeLeftover(t *testing.T, root *windowsV3Directory, name string) {
	t.Helper()
	probe, err := root.CreatePrivateDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := probe.CreatePrivateFile("stage")
	if err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := stage.Truncate(1); err != nil {
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	anchor, err := probe.LinkRegularFileNoReplace(stage, "anchor")
	if err != nil {
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	publication, err := probe.LinkRegularFileNoReplace(anchor, "publication")
	if err != nil {
		_ = anchor.Close()
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	record, err := probe.CreatePrivateFile("record")
	if err != nil {
		_ = publication.Close()
		_ = anchor.Close()
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := record.Truncate(1); err != nil {
		_ = record.Close()
		_ = publication.Close()
		_ = anchor.Close()
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	installed, err := probe.CreatePrivateDirectory("installed")
	if err != nil {
		_ = record.Close()
		_ = publication.Close()
		_ = anchor.Close()
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	candidate, err := probe.CreatePrivateDirectory("candidate")
	if err != nil {
		_ = installed.Close()
		_ = record.Close()
		_ = publication.Close()
		_ = anchor.Close()
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := errors.Join(
		stage.Sync(), record.Sync(), installed.Sync(), candidate.Sync(), probe.Sync(), root.Sync(),
	); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(
		candidate.Close(), installed.Close(), record.Close(), publication.Close(), anchor.Close(), stage.Close(), probe.Close(),
	); err != nil {
		t.Fatal(err)
	}
}
