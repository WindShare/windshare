//go:build linux

package outputlinux

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxOutputProbeRetriesCollisionAndCleansNativeNamespace(t *testing.T) {
	root, rootPath := openLinuxOutputProbeCoverageRoot(t)
	const (
		firstName  = outputprobe.NamePrefix + "11111111111111111111111111111111"
		secondName = outputprobe.NamePrefix + "22222222222222222222222222222222"
	)
	system := linuxHostOutputSystem
	originalMkdirat := system.mkdirat
	var allocations []string
	system.mkdirat = func(fd int, name string, mode uint32) error {
		switch name {
		case firstName:
			allocations = append(allocations, name)
			return unix.EEXIST
		case secondName:
			allocations = append(allocations, name)
		}
		return originalMkdirat(fd, name, mode)
	}
	root.system = &system

	entropy := append(bytes.Repeat([]byte{0x11}, outputprobe.RandomBytes),
		bytes.Repeat([]byte{0x22}, outputprobe.RandomBytes)...)
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(entropy)); err != nil {
		t.Fatalf("exercise native feature probe: %v", err)
	}
	if want := []string{firstName, secondName}; !reflect.DeepEqual(allocations, want) {
		t.Fatalf("probe allocations = %v, want %v", allocations, want)
	}
	assertLinuxProbeCoverageRootEmpty(t, rootPath)

	if err := root.probeRecoverableFeaturesWithRandom(nil); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("nil entropy source error = %v, want unsafe", err)
	}
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("exhausted entropy source error = %v, want EOF", err)
	}
	assertLinuxProbeCoverageRootEmpty(t, rootPath)
}

func TestLinuxRangeProbeFailurePreservesLiveOutput(t *testing.T) {
	for _, afterMutation := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-mutation=%t", afterMutation), func(t *testing.T) {
			root, rootPath := openLinuxOutputProbeCoverageRoot(t)
			system := linuxHostOutputSystem
			rename := system.renameat2
			injected := false
			system.renameat2 = func(sourceFD int, source string, targetFD int, target string, flags uint) error {
				if source != outputprobe.TemporaryRecordName || target != outputprobe.RecordName {
					return rename(sourceFD, source, targetFD, target, flags)
				}
				injected = true
				if afterMutation {
					if err := rename(sourceFD, source, targetFD, target, flags); err != nil {
						return err
					}
				}
				return unix.EIO
			}
			root.system = &system
			capabilities, err := root.destinationCapabilitiesWithRandom(
				bytes.NewReader(bytes.Repeat([]byte{0x67}, outputprobe.RandomBytes)))
			if err != nil {
				t.Fatalf("record replacement failure rejected safe output: %v", err)
			}
			if !injected || capabilities.RangeRecovery().Supported() ||
				!capabilities.SafePublish().Supported() || !capabilities.OperationRecovery().Supported() ||
				!capabilities.CrashCleanup().Supported() {
				t.Fatalf("replacement failure changed unrelated capabilities: injected=%t capabilities=%+v", injected, capabilities)
			}
			mode, err := outputcap.SelectExecutionMode(capabilities)
			if err != nil || mode != outputcap.ExecutionLiveOnly {
				t.Fatalf("safe output mode=%v error=%v", mode, err)
			}
			assertLinuxProbeCoverageRootEmpty(t, rootPath)
		})
	}
}

func TestLinuxOutputProbeRecoveryRemovesReachableCutInProtocolOrder(t *testing.T) {
	root, rootPath := openLinuxOutputProbeCoverageRoot(t)
	const probeName = outputprobe.NamePrefix + "33333333333333333333333333333333"
	probePath := createLinuxProbeCoverageDirectory(t, rootPath, probeName)
	writeLinuxProbeCoverageFile(t, probePath, "live-stage", nil)
	stagePath := writeLinuxProbeCoverageFile(t, probePath, "stage", nil)
	for _, name := range []string{"anchor", "publication"} {
		if err := os.Link(stagePath, filepath.Join(probePath, name)); err != nil {
			t.Fatalf("link probe %q: %v", name, err)
		}
	}
	writeLinuxProbeCoverageFile(t, probePath, "record", []byte{1})
	for _, name := range []string{"candidate", "installed"} {
		createLinuxProbeCoverageDirectory(t, probePath, name)
	}

	system := linuxHostOutputSystem
	originalUnlinkat := system.unlinkat
	var removals []string
	system.unlinkat = func(fd int, name string, flags int) error {
		removals = append(removals, name)
		return originalUnlinkat(fd, name, flags)
	}
	root.system = &system
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		t.Fatalf("recover reachable probe cut: %v", err)
	}
	want := []string{"live-stage", "stage", "publication", "anchor", "record", "candidate", "installed", probeName}
	if !reflect.DeepEqual(removals, want) {
		t.Fatalf("probe removal order = %v, want %v", removals, want)
	}
	assertLinuxProbeCoverageRootEmpty(t, rootPath)
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		t.Fatalf("repeat empty recovery: %v", err)
	}
}

func TestLinuxOutputProbeLeftoverRejectsAmbiguousNativeCuts(t *testing.T) {
	for index, test := range []struct {
		name  string
		build func(*testing.T, string)
	}{
		{name: "unexpected entry", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "unexpected", nil)
		}},
		{name: "file where directory is required", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "candidate", nil)
		}},
		{name: "directory where file is required", build: func(t *testing.T, probePath string) {
			createLinuxProbeCoverageDirectory(t, probePath, "stage")
		}},
		{name: "nonempty candidate", build: func(t *testing.T, probePath string) {
			candidate := createLinuxProbeCoverageDirectory(t, probePath, "candidate")
			writeLinuxProbeCoverageFile(t, candidate, "child", nil)
		}},
		{name: "mismatched data identities", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "stage", nil)
			writeLinuxProbeCoverageFile(t, probePath, "anchor", nil)
		}},
		{name: "unreachable publication", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "publication", nil)
		}},
		{name: "nonzero Linux data link", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "stage", []byte{1})
		}},
		{name: "record paired with unreachable data prefix", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "stage", nil)
			writeLinuxProbeCoverageFile(t, probePath, "record", nil)
		}},
		{name: "invalid record generation", build: func(t *testing.T, probePath string) {
			writeLinuxProbeCoverageFile(t, probePath, "record", []byte{1, 2})
		}},
		{name: "nonprivate record mode", build: func(t *testing.T, probePath string) {
			path := writeLinuxProbeCoverageFile(t, probePath, "record", nil)
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, rootPath := openLinuxOutputProbeCoverageRoot(t)
			probeName := outputprobe.NamePrefix + fmt.Sprintf("%032x", index+1)
			probePath := createLinuxProbeCoverageDirectory(t, rootPath, probeName)
			test.build(t, probePath)
			leftover, err := outputprobe.Inspect(&linuxDirectory{native: root}, probeName, outputprobe.LinuxExt4)
			if leftover != nil {
				_ = leftover.Close()
			}
			if err == nil || leftover != nil {
				t.Fatalf("ambiguous probe cut = leftover %v, error %v", leftover, err)
			}
			if _, statErr := os.Stat(probePath); statErr != nil {
				t.Fatalf("strict inspection mutated rejected probe: %v", statErr)
			}
		})
	}
}

func TestLinuxOutputProbeLeftoverRemovalRejectsNameReplacement(t *testing.T) {
	root, rootPath := openLinuxOutputProbeCoverageRoot(t)
	const probeName = outputprobe.NamePrefix + "44444444444444444444444444444444"
	probePath := createLinuxProbeCoverageDirectory(t, rootPath, probeName)
	recordPath := writeLinuxProbeCoverageFile(t, probePath, "record", []byte{1})
	leftover, err := outputprobe.Inspect(&linuxDirectory{native: root}, probeName, outputprobe.LinuxExt4)
	if err != nil {
		t.Fatalf("inspect reachable record cut: %v", err)
	}
	defer leftover.Close()
	displacedPath := filepath.Join(probePath, "displaced-record")
	if err := os.Rename(recordPath, displacedPath); err != nil {
		t.Fatal(err)
	}
	want := []byte("replacement")
	writeLinuxProbeCoverageFile(t, probePath, "record", want)

	if err := leftover.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("remove replaced probe record error = %v, want unsafe", err)
	}
	if got, err := os.ReadFile(recordPath); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("replacement record = %q, %v; want %q", got, err, want)
	}
	if _, err := os.Stat(displacedPath); err != nil {
		t.Fatalf("original record was not retained through its open authority: %v", err)
	}
	if err := (*outputprobe.Workspace)(nil).Close(); err != nil {
		t.Fatalf("close nil leftover: %v", err)
	}
}

func TestLinuxOutputProbeRecoveryFailsClosedOnReservedNamespaceBounds(t *testing.T) {
	t.Run("malformed reserved name", func(t *testing.T) {
		root, rootPath := openLinuxOutputProbeCoverageRoot(t)
		name := outputprobe.ReservedPrefix + "-not-canonical"
		path := createLinuxProbeCoverageDirectory(t, rootPath, name)
		if err := root.recoverOutputProbeLeftovers(); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Fatalf("malformed reserved name error = %v, want unsafe", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("malformed reserved namespace was mutated: %v", err)
		}
	})

	t.Run("leftover count", func(t *testing.T) {
		root, rootPath := openLinuxOutputProbeCoverageRoot(t)
		for index := 0; index <= outputprobe.MaximumLeftovers; index++ {
			name := outputprobe.NamePrefix + fmt.Sprintf("%032x", index+1)
			createLinuxProbeCoverageDirectory(t, rootPath, name)
		}
		if err := root.recoverOutputProbeLeftovers(); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Fatalf("excess leftover count error = %v, want unsafe", err)
		}
		entries, err := os.ReadDir(rootPath)
		if err != nil || len(entries) != outputprobe.MaximumLeftovers+1 {
			t.Fatalf("failed count check mutated leftovers: entries=%d error=%v", len(entries), err)
		}
	})
}

func openLinuxOutputProbeCoverageRoot(t *testing.T) (*linuxOutputDirectory, string) {
	t.Helper()
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	root, err := linuxOpenExt4OutputRoot(rootPath, &linuxHostOutputSystem)
	if err != nil {
		if errors.Is(err, errLinuxOutputUnsupported) {
			t.Skipf("test volume is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatalf("open certified Linux output root: %v", err)
	}
	t.Cleanup(func() {
		if err := root.close(); err != nil {
			t.Errorf("close certified Linux output root: %v", err)
		}
	})
	return root, rootPath
}

func createLinuxProbeCoverageDirectory(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, linuxOutputDirectoryMode); err != nil {
		t.Fatalf("create probe directory %q: %v", name, err)
	}
	return path
}

func writeLinuxProbeCoverageFile(t *testing.T, parent, name string, payload []byte) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.WriteFile(path, payload, linuxOutputStateFileMode); err != nil {
		t.Fatalf("create probe file %q: %v", name, err)
	}
	return path
}

func assertLinuxProbeCoverageRootEmpty(t *testing.T, rootPath string) {
	t.Helper()
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe root entries = %v, error = %v", entries, err)
	}
}

func TestLinuxOutputProbeRootLockOperations(t *testing.T) {
	root, _ := openLinuxOutputProbeCoverageRoot(t)
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		t.Fatalf("acquireOutputProbeLock error = %v", err)
	}
	if lock == nil || lock.directory == nil {
		t.Fatal("acquired lock is nil")
	}

	// Double lock should fail with busy error
	if _, err := root.acquireOutputProbeLock(); !errors.Is(err, errLinuxOutputLockBusy) {
		t.Fatalf("concurrent probe lock error = %v, want busy", err)
	}

	// Release lock
	if err := root.releaseOutputProbeLock(lock); err != nil {
		t.Fatalf("releaseOutputProbeLock error = %v", err)
	}

	// Release nil lock fails closed
	if err := root.releaseOutputProbeLock(nil); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("nil lock release error = %v, want unsafe", err)
	}
	if err := root.releaseOutputProbeLock(&linuxOutputProbeRootLock{}); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("empty lock release error = %v, want unsafe", err)
	}
}
