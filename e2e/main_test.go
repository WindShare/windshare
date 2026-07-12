package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/internal/testnetwork"
)

// 编译产物一次性构建、全套件共享(§11 T6.1 基础设施要求):进程编排的所有
// 用例都指向这两个二进制,避免每个用例重复 go build 的秒级开销。
var (
	relayBin     string
	windshareBin string
)

// TestMain does not even materialize child executables on Windows. The shared
// process constructors also enforce the OS-network gate, but avoiding the build
// makes the platform boundary obvious and removes random executable identities.
func TestMain(m *testing.M) {
	if !testnetwork.OSNetworkEnabled() {
		os.Exit(m.Run())
	}
	dir, cleanup, err := e2eBuildDirectory()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: create build directory:", err)
		os.Exit(1)
	}
	if err := buildBinaries(dir); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		cleanup()
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func e2eBuildDirectory() (string, func(), error) {
	if dir, ok := stableE2EBuildDirectory(
		repoRoot(),
		runtime.GOOS,
		testnetwork.StableHarness(),
	); ok {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", func() {}, err
		}
		return dir, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "windshare-e2e-bin")
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func stableE2EBuildDirectory(root, goos string, runnerAuthorized bool) (string, bool) {
	if !testnetwork.StableHarnessFor(goos, runnerAuthorized) {
		return "", false
	}
	return filepath.Join(root, "tmp", "d5-harness", "e2e-bin"), true
}

func TestStableE2EBuildDirectorySelection(t *testing.T) {
	t.Parallel()
	root := filepath.Join("workspace", "windshare")
	want := filepath.Join(root, "tmp", "d5-harness", "e2e-bin")
	tests := []struct {
		name       string
		goos       string
		authorized bool
		wantPath   string
		wantOK     bool
	}{
		{name: "verified Windows harness", goos: "windows", authorized: true, wantPath: want, wantOK: true},
		{name: "ordinary Windows test", goos: "windows"},
		{name: "non-Windows ignores authorization", goos: "linux", authorized: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := stableE2EBuildDirectory(root, test.goos, test.authorized)
			if got != test.wantPath || ok != test.wantOK {
				t.Fatalf("stable directory = %q, %v; want %q, %v", got, ok, test.wantPath, test.wantOK)
			}
		})
	}
}

// buildBinaries 经 go build 把两个 cmd 包编译进 outDir。工作目录设为仓库根
// (go.work 所在),使双模块 monorepo 的 core 依赖经 workspace 解析(§6.2)。
func buildBinaries(outDir string) error {
	testnetwork.AssertOSNetwork()
	root := repoRoot()
	targets := []struct {
		out string
		pkg string
	}{
		{filepath.Join(outDir, exeName("wsrelay")), "./relay/cmd/wsrelay"},
		{filepath.Join(outDir, exeName("windshare")), "./cmd/windshare"},
	}
	if testnetwork.StableHarness() {
		for _, target := range targets {
			if err := testnetwork.VerifyAuthorizedExecutable(target.out); err != nil {
				return fmt.Errorf("verify runner-built %s: %w", target.pkg, err)
			}
		}
		relayBin = targets[0].out
		windshareBin = targets[1].out
		return nil
	}
	for _, tgt := range targets {
		args := e2eBuildArguments(
			runtime.GOOS,
			false,
			tgt.out,
			tgt.pkg,
		)
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %v\n%s", tgt.pkg, err, out)
		}
	}
	relayBin = targets[0].out
	windshareBin = targets[1].out
	return nil
}

func e2eBuildArguments(goos string, runnerAuthorized bool, output, pkg string) []string {
	arguments := []string{"build"}
	if testnetwork.StableHarnessFor(goos, runnerAuthorized) {
		arguments = append(arguments, "-race")
	}
	return append(arguments, "-o", output, pkg)
}

func TestE2EBuildArgumentsUseRaceOnlyForStableWindowsRunner(t *testing.T) {
	t.Parallel()
	windows := e2eBuildArguments("windows", true, "out.exe", "./cmd")
	if len(windows) < 2 || windows[1] != "-race" {
		t.Fatalf("stable Windows build arguments = %v, want -race", windows)
	}
	linux := e2eBuildArguments("linux", true, "out", "./cmd")
	if len(linux) > 1 && linux[1] == "-race" {
		t.Fatalf("ordinary Linux build unexpectedly changed: %v", linux)
	}
}

// repoRoot 由本测试文件的编译期绝对路径回溯:e2e/ 的父目录即仓库根。构建与
// 运行同机,故源码路径有效。
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("e2e: locate test source path")
	}
	return filepath.Dir(filepath.Dir(file))
}

// exeName 在 Windows 上补 .exe 后缀。
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
