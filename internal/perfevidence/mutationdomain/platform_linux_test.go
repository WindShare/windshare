//go:build linux

package mutationdomain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
	"golang.org/x/sys/unix"
)

const linuxPrivilegeProbeEnvironment = "WINDSHARE_LINUX_PRIVILEGE_PROBE"

const (
	linuxDaemonProbeEnvironment = "WINDSHARE_LINUX_DAEMON_PROBE"
	linuxForkProbeEnvironment   = "WINDSHARE_LINUX_FORK_PROBE"
	linuxForkProbeChildren      = 128
)

func TestLinuxTargetCannotRecoverNamespaceMountAuthority(t *testing.T) {
	domain, target, inputRoot := openLinuxTestDomain(t)
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestLinuxPrivilegeProbeTarget$"},
		Directory:  inputRoot,
		Environment: []string{
			linuxPrivilegeProbeEnvironment + "=1",
		},
	}, nil)
	if err != nil {
		t.Fatalf("run Linux namespace privilege probe: %v; stderr=%s", err, result.Stderr)
	}
	if result.ExitCode != 0 || result.ProcessID <= 2 || !strings.Contains(string(result.Stdout), "namespace-privileges-dropped") {
		t.Fatalf("Linux privilege probe = exit %d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestLinuxSettlesDaemonHoldingCapturePipesBeforeReuse(t *testing.T) {
	domain, target, inputRoot := openLinuxTestDomain(t)
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target, Arguments: []string{"-test.run=^TestLinuxDaemonProbeTarget$"}, Directory: inputRoot,
		Environment: []string{linuxDaemonProbeEnvironment + "=root"},
	}, nil)
	if err != nil {
		t.Fatalf("daemon settlement: %v; stderr=%s", err, result.Stderr)
	}
	if result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "daemon-descendant-started") {
		t.Fatalf("daemon result = exit %d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	result, err = domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target, Arguments: []string{"-test.run=^TestLinuxQuickTarget$"}, Directory: inputRoot,
		Environment: []string{linuxDaemonProbeEnvironment + "=quick"},
	}, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "namespace-reused-cleanly") {
		t.Fatalf("post-daemon reuse = exit %d stdout=%q stderr=%q err=%v", result.ExitCode, result.Stdout, result.Stderr, err)
	}
}

func TestLinuxSettlesForkSwarmBeforeReturning(t *testing.T) {
	domain, target, inputRoot := openLinuxTestDomain(t)
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target, Arguments: []string{"-test.run=^TestLinuxForkProbeTarget$"}, Directory: inputRoot,
		Environment: []string{linuxForkProbeEnvironment + "=root"},
	}, nil)
	if err != nil {
		t.Fatalf("fork-swarm settlement: %v; stderr=%s", err, result.Stderr)
	}
	if result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "fork-swarm-started") {
		t.Fatalf("fork-swarm result = exit %d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func openLinuxTestDomain(t *testing.T) (perfevidence.MutationDomain, string, string) {
	t.Helper()
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "target.test")
	if err := copyRegularTestFile(executable, target); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatalf("open Linux namespace mutation domain: %v", err)
	}
	return domain, target, inputRoot
}

func TestLinuxPrivilegeProbeTarget(t *testing.T) {
	if os.Getenv(linuxPrivilegeProbeEnvironment) != "1" {
		t.Skip("target-only Linux privilege probe")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"CapInh:", "CapPrm:", "CapEff:", "CapAmb:"} {
		if !strings.Contains(string(status), field+"\t0000000000000000") {
			t.Fatalf("%s was not cleared:\n%s", field, status)
		}
	}
	if !strings.Contains(string(status), "NoNewPrivs:\t1") {
		t.Fatalf("no_new_privs was not set:\n%s", status)
	}
	if err := unix.Mount("", "/inputs", "", unix.MS_REMOUNT, ""); !errors.Is(err, unix.EPERM) {
		t.Fatalf("remount immutable inputs error = %v, want EPERM", err)
	}
	if err := unix.Mount("tmpfs", "/temporary", "tmpfs", 0, ""); !errors.Is(err, unix.EPERM) {
		t.Fatalf("mount replacement error = %v, want EPERM", err)
	}
	oldRoot := "/temporary/pivot-old-root"
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.PivotRoot("/", oldRoot); !errors.Is(err, unix.EPERM) {
		t.Fatalf("pivot_root error = %v, want EPERM", err)
	}
	if err := os.WriteFile("/inputs/test/forbidden", []byte("tamper"), 0o600); !errors.Is(err, unix.EROFS) {
		t.Fatalf("immutable input write error = %v, want EROFS", err)
	}
	_, _ = fmt.Fprint(os.Stdout, "namespace-privileges-dropped")
}

func TestLinuxDaemonProbeTarget(t *testing.T) {
	switch os.Getenv(linuxDaemonProbeEnvironment) {
	case "":
		t.Skip("target-only daemon probe")
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestLinuxDaemonProbeTarget$")
		child.Env = []string{linuxDaemonProbeEnvironment + "=child"}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprint(os.Stdout, "daemon-descendant-started")
	default:
		t.Fatalf("unknown daemon probe role")
	}
}

func TestLinuxQuickTarget(t *testing.T) {
	if os.Getenv(linuxDaemonProbeEnvironment) != "quick" {
		t.Skip("target-only reuse probe")
	}
	_, _ = fmt.Fprint(os.Stdout, "namespace-reused-cleanly")
}

func TestLinuxForkProbeTarget(t *testing.T) {
	switch os.Getenv(linuxForkProbeEnvironment) {
	case "":
		t.Skip("target-only fork probe")
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	case "root":
		for index := range linuxForkProbeChildren {
			child := exec.Command(os.Args[0], "-test.run=^TestLinuxForkProbeTarget$")
			child.Env = []string{linuxForkProbeEnvironment + "=child"}
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatalf("start fork child %d: %v", index, err)
			}
		}
		_, _ = fmt.Fprint(os.Stdout, "fork-swarm-started")
	default:
		t.Fatalf("unknown fork probe role")
	}
}
