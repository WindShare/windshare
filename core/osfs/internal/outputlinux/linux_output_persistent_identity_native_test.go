//go:build linux

package outputlinux

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	linuxNativeReuseRootEnvironment = "WINDSHARE_LINUX_NATIVE_REUSE_ROOT"
	linuxNativeRestartWorkerMode    = "WINDSHARE_LINUX_RESTART_IDENTITY_WORKER"
	linuxNativeReuseCycles          = 32
	linuxNativeMaximumFixtureInodes = 4096
)

type linuxNativeRestartObservation struct {
	RootBinding       string `json:"rootBinding"`
	FilesystemUUID    string `json:"filesystemUUID"`
	Inode             uint64 `json:"inode"`
	BirthSeconds      int64  `json:"birthSeconds"`
	BirthNanoseconds  uint32 `json:"birthNanoseconds"`
	Generation        uint32 `json:"generation"`
	GenerationPresent bool   `json:"generationPresent"`
}

func TestLinuxExt4RestartIdentityRejectsForcedInodeReuse(t *testing.T) {
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == "" {
		t.Skip("forced ext4 inode reuse runs only in the required native fixture")
	}
	requireUnprivilegedLinuxExt4Certification(t)
	if os.Getenv(linuxNativeFixtureEnvironment) != linuxNativeFixtureVersion {
		t.Fatal("required Linux/ext4 certification is outside the loop-ext4 fixture")
	}
	reuseRoot := os.Getenv(linuxNativeReuseRootEnvironment)
	if !filepath.IsAbs(reuseRoot) || filepath.Clean(reuseRoot) != reuseRoot {
		t.Fatalf("native reuse root is not clean and absolute: %q", reuseRoot)
	}
	fillerRoot := filepath.Join(reuseRoot, "fillers")
	target := filepath.Join(reuseRoot, "target")
	assertLinuxNativeEmptyDirectory(t, fillerRoot)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native reuse target must start absent: %v", err)
	}

	baseline := linuxNativeFreeInodes(t, reuseRoot)
	createdFillers := make([]string, 0, linuxNativeMaximumFixtureInodes)
	t.Cleanup(func() {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove native reuse target: %v", err)
		}
		for index := len(createdFillers) - 1; index >= 0; index-- {
			if err := os.Remove(createdFillers[index]); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove inode filler %q: %v", createdFillers[index], err)
			}
		}
		if restored := linuxNativeFreeInodes(t, reuseRoot); restored != baseline {
			t.Errorf("free inode baseline was not restored: got %d, want %d", restored, baseline)
		}
	})

	if err := os.Mkdir(target, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	freeAfterTarget := linuxNativeFreeInodes(t, reuseRoot)
	if freeAfterTarget == 0 || freeAfterTarget > linuxNativeMaximumFixtureInodes {
		t.Fatalf("fixture free inode count %d is outside deterministic bound", freeAfterTarget)
	}
	for index := range freeAfterTarget {
		name := filepath.Join(fillerRoot, fmt.Sprintf("inode-%04d", index))
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, linuxOutputStateFileMode)
		if err != nil {
			t.Fatalf("consume fixture inode %d/%d: %v", index+1, freeAfterTarget, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close inode filler: %v", err)
		}
		createdFillers = append(createdFillers, name)
	}
	if free := linuxNativeFreeInodes(t, reuseRoot); free != 0 {
		t.Fatalf("fixture did not exhaust its inode pool: %d remain", free)
	}

	initial := observeLinuxNativeRestartIdentity(t, target)
	unchanged := observeLinuxNativeRestartIdentity(t, target)
	if initial != unchanged {
		t.Fatalf("unchanged directory changed across processes:\nfirst=%+v\nsecond=%+v", initial, unchanged)
	}
	seenBirthTimes := map[string]struct{}{linuxNativeBirthTimeKey(initial): {}}
	previous := initial
	for cycle := range linuxNativeReuseCycles {
		if err := os.Remove(target); err != nil {
			t.Fatalf("cycle %d remove target: %v", cycle, err)
		}
		if free := linuxNativeFreeInodes(t, reuseRoot); free != 1 {
			t.Fatalf("cycle %d deletion exposed %d free inodes, want 1", cycle, free)
		}
		if err := os.Mkdir(target, linuxOutputDirectoryMode); err != nil {
			t.Fatalf("cycle %d recreate target: %v", cycle, err)
		}
		if free := linuxNativeFreeInodes(t, reuseRoot); free != 0 {
			t.Fatalf("cycle %d recreation left %d free inodes", cycle, free)
		}

		observed := observeLinuxNativeRestartIdentity(t, target)
		if observed.Inode != initial.Inode {
			t.Fatalf("cycle %d did not force inode reuse: got %d, want %d",
				cycle, observed.Inode, initial.Inode)
		}
		birthKey := linuxNativeBirthTimeKey(observed)
		if _, duplicate := seenBirthTimes[birthKey]; duplicate {
			t.Fatalf("cycle %d reused inode %d with repeated STATX_BTIME %s",
				cycle, observed.Inode, birthKey)
		}
		seenBirthTimes[birthKey] = struct{}{}
		if observed.RootBinding == initial.RootBinding || observed.RootBinding == previous.RootBinding {
			t.Fatalf("cycle %d production root binding accepted a replacement directory", cycle)
		}
		if observed.FilesystemUUID != initial.FilesystemUUID {
			t.Fatalf("cycle %d escaped the fixed ext4 filesystem", cycle)
		}
		previous = observed
	}
}

func TestLinuxExt4RestartIdentityWorker(t *testing.T) {
	if os.Getenv(linuxNativeRestartWorkerMode) != "observe" {
		t.Skip("helper process")
	}
	assertLinuxNativeWorkerBoundary(t)
	path := os.Getenv(linuxNativeReuseRootEnvironment)
	platformValue, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	platform, ok := platformValue.(*linuxV3Platform)
	if !ok {
		t.Fatalf("unexpected native platform type %T", platformValue)
	}
	defer func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close native restart worker platform: %v", err)
		}
	}()
	binding, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	identity := platform.root.native.certificate.rootRestartIdentity
	observation := linuxNativeRestartObservation{
		RootBinding: binding.String(), FilesystemUUID: fmt.Sprintf("%x", identity.mount.filesystemUUID),
		Inode: identity.inode, BirthSeconds: identity.birthSeconds,
		BirthNanoseconds: identity.birthNanoseconds, Generation: identity.generation,
		GenerationPresent: identity.hasGenerationProof,
	}
	output := os.NewFile(3, "restart-identity-observation")
	if output == nil {
		t.Fatal("restart identity observation pipe is absent")
	}
	if err := json.NewEncoder(output).Encode(observation); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func observeLinuxNativeRestartIdentity(t *testing.T, path string) linuxNativeRestartObservation {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLinuxExt4RestartIdentityWorker$")
	// The native chroot intentionally has no device nodes. An EOF-backed pipe
	// keeps os/exec from opening /dev/null before it starts the worker.
	command.Stdin = bytes.NewReader(nil)
	command.Env = append(os.Environ(),
		linuxNativeRestartWorkerMode+"=observe",
		linuxNativeReuseRootEnvironment+"="+path,
	)
	command.ExtraFiles = []*os.File{writer}
	var diagnostics bytes.Buffer
	command.Stdout = &diagnostics
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	var observation linuxNativeRestartObservation
	decodeErr := json.NewDecoder(reader).Decode(&observation)
	closeErr := reader.Close()
	waitErr := command.Wait()
	if decodeErr != nil || closeErr != nil || waitErr != nil {
		t.Fatalf("restart identity worker failed: decode=%v close=%v wait=%v output=%s",
			decodeErr, closeErr, waitErr, diagnostics.String())
	}
	if observation.RootBinding == "" || observation.FilesystemUUID == "" || observation.Inode == 0 {
		t.Fatalf("restart identity worker returned incomplete observation: %+v", observation)
	}
	return observation
}

func assertLinuxNativeWorkerBoundary(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatal("native identity worker retained root identity")
	}
	status, err := os.ReadFile(linuxProcessStatusPath)
	if err != nil {
		t.Fatal(err)
	}
	capabilityHex := ""
	for line := range strings.SplitSeq(string(status), "\n") {
		if value, found := strings.CutPrefix(line, "CapEff:"); found {
			capabilityHex = strings.TrimSpace(value)
			break
		}
	}
	capabilities, err := strconv.ParseUint(capabilityHex, 16, 64)
	if err != nil || capabilities != 0 {
		t.Fatalf("native identity worker CapEff=%q parsed=%#x error=%v",
			capabilityHex, capabilities, err)
	}
}

func assertLinuxNativeEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("native fixture directory %q is not empty", path)
	}
}

func linuxNativeFreeInodes(t *testing.T, path string) uint64 {
	t.Helper()
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Ffree
}

func linuxNativeBirthTimeKey(observation linuxNativeRestartObservation) string {
	return fmt.Sprintf("%d.%09d", observation.BirthSeconds, observation.BirthNanoseconds)
}
