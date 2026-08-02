//go:build linux

package testprocess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

const (
	linuxParentDeathHelperEnvironment   = "WINDSHARE_LINUX_PARENT_DEATH_OWNER"
	linuxParentDeathManifestEnvironment = "WINDSHARE_LINUX_PARENT_DEATH_MANIFEST"
	linuxParentDeathHarnessEnvironment  = "WINDSHARE_LINUX_PARENT_DEATH_HARNESS"
	linuxParentDeathTargetEnvironment   = "WINDSHARE_LINUX_PARENT_DEATH_TARGET"
	linuxParentDeathSurvivorEnvironment = "WINDSHARE_LINUX_PARENT_DEATH_SURVIVOR"
	linuxOutputFloodTargetEnvironment   = "WINDSHARE_LINUX_OUTPUT_FLOOD_TARGET"
	linuxOutputFloodBytes               = MaximumCapturedOutputBytes + (1 << 20)
)

func TestLinuxOwnerLaunchUsesIndependentSignalDomainAndNeutralDirectory(t *testing.T) {
	helperPath := filepath.Join(t.TempDir(), "owner")
	command := exec.Command(helperPath, "guard")
	configureLinuxOwnerCommand(command, helperPath)
	if command.Dir != filepath.Dir(helperPath) {
		t.Fatalf("owner working directory = %q", command.Dir)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid || command.SysProcAttr.Setpgid {
		t.Fatalf("owner signal-domain configuration = %#v", command.SysProcAttr)
	}
}

func TestLinuxOwnerJoinUsesEOFRetirementWithoutKillingOwner(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusWriter.Close()
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	session := &linuxSession{
		controlWriter: controlWriter,
		statusReader:  statusReader,
		status:        make(chan linuxStatusResult),
		waitResult:    make(chan error),
		lifecycleEnd:  time.Now().Add(10 * time.Millisecond),
		retireBudget:  10 * time.Millisecond,
	}
	started := time.Now()
	if _, err := session.wait(); err == nil || !strings.Contains(err.Error(), "bounded lifecycle") {
		t.Fatalf("bounded join error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded join took %s", elapsed)
	}
	buffer := make([]byte, 1)
	if count, err := controlReader.Read(buffer); count != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("control authority after retirement = bytes=%d err=%v", count, err)
	}
}

func TestLinuxOutputCaptureLimitDoesNotPerturbNaturalSettlement(t *testing.T) {
	helperPath := linuxParentDeathHelper(t)
	owner, err := NewOwner(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close output-drain owner fixture: %v", err)
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), Spec{
		Identity: protocol.Identity{
			RunID: "linux-output-drain", OperationID: "capture-limit", Scenario: "natural-settlement",
		},
		Command: Command{
			Executable:       executable,
			Arguments:        []string{"-test.run=^TestLinuxOutputFloodTarget$", "-test.count=1"},
			WorkingDirectory: filepath.Dir(executable),
			Environment: []protocol.EnvironmentEntry{
				{Name: linuxOutputFloodTargetEnvironment, Value: "1"},
			},
		},
		Deadline: 5 * time.Second, TerminationGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settlement, waitErr := process.Wait(waitContext)
	if !errors.Is(waitErr, ErrOutputCaptureLimit) || !strings.Contains(waitErr.Error(), "stdout") {
		t.Fatalf("output diagnostic = %v", waitErr)
	}
	output := process.Stdout()
	if !output.Truncated || len(output.Bytes) != MaximumCapturedOutputBytes {
		t.Fatalf("bounded stdout snapshot = bytes=%d truncated=%t", len(output.Bytes), output.Truncated)
	}
	if settlement.TerminationReason != protocol.TerminationNatural ||
		settlement.Target.Outcome != protocol.TargetExited ||
		settlement.Target.ExitCode == nil || *settlement.Target.ExitCode != 0 ||
		settlement.TreeState != protocol.TreeProvenEmpty ||
		settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		t.Fatalf("settlement after output truncation = %#v", settlement)
	}
}

func TestLinuxOutputFloodTarget(t *testing.T) {
	if os.Getenv(linuxOutputFloodTargetEnvironment) != "1" {
		t.Skip("invoked only as an owned output-flood target")
	}
	payload := make([]byte, linuxOutputFloodBytes)
	written, err := os.Stdout.Write(payload)
	if err != nil || written != len(payload) {
		t.Fatalf("write output flood: bytes=%d err=%v", written, err)
	}
}

func TestLinuxOwnerSurvivesParentGroupDeathAndRetiresTree(t *testing.T) {
	helperPath := linuxParentDeathHelper(t)
	manifestPath := filepath.Join(t.TempDir(), "owned-tree.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(executable, "-test.run=^TestLinuxParentDeathHarness$", "-test.count=1")
	parent.Env = append(os.Environ(),
		linuxParentDeathHarnessEnvironment+"=1",
		linuxParentDeathHelperEnvironment+"="+helperPath,
		linuxParentDeathManifestEnvironment+"="+manifestPath,
	)
	parent.Stdout = os.Stdout
	parent.Stderr = os.Stderr
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parentWaited := false
	t.Cleanup(func() {
		if parentWaited {
			return
		}
		// The unreaped parent still owns its numeric process-group identity here.
		_ = unix.Kill(-parent.Process.Pid, unix.SIGKILL)
		_ = parent.Wait()
	})
	manifest := waitForLinuxParentDeathManifest(t, manifestPath)
	if sessionID, err := unix.Getsid(manifest.GuardianPID); err != nil || sessionID != manifest.GuardianPID {
		t.Fatalf("guardian session = %d, %v; guardian pid=%d", sessionID, err, manifest.GuardianPID)
	}
	if sessionID, err := unix.Getsid(manifest.OwnerPID); err != nil || sessionID != manifest.GuardianPID {
		t.Fatalf("owner session = %d, %v; guardian pid=%d owner pid=%d", sessionID, err, manifest.GuardianPID, manifest.OwnerPID)
	}
	pidfds := make(map[string]int, 4)
	for label, pid := range map[string]int{
		"guardian": manifest.GuardianPID, "owner": manifest.OwnerPID,
		"root": manifest.RootPID, "setsid survivor": manifest.SurvivorPID,
	} {
		pidfd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatalf("open %s pidfd for %d: %v", label, pid, err)
		}
		pidfds[label] = pidfd
		defer unix.Close(pidfd)
	}
	if err := unix.Kill(-parent.Process.Pid, unix.SIGKILL); err != nil {
		t.Fatalf("kill parent process group: %v", err)
	}
	_ = parent.Wait()
	parentWaited = true
	for label, pidfd := range pidfds {
		poll := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		count, err := unix.Poll(poll, 5_000)
		if err != nil || count != 1 || poll[0].Revents&unix.POLLIN == 0 {
			t.Fatalf("%s did not retire after parent group death: count=%d event=%#x err=%v", label, count, poll[0].Revents, err)
		}
	}
}

func TestLinuxParentDeathHarness(t *testing.T) {
	if os.Getenv(linuxParentDeathHarnessEnvironment) != "1" {
		t.Skip("invoked only as an intermediary parent-death fixture")
	}
	owner, err := NewOwner(os.Getenv(linuxParentDeathHelperEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Start(context.Background(), Spec{
		Identity: protocol.Identity{
			RunID: "linux-parent-death", OperationID: "intermediary-parent", Scenario: "group-death",
		},
		Command: Command{
			Executable:       executable,
			Arguments:        []string{"-test.run=^TestLinuxParentDeathTarget$", "-test.count=1"},
			WorkingDirectory: filepath.Dir(executable),
			Environment: []protocol.EnvironmentEntry{
				{Name: linuxParentDeathManifestEnvironment, Value: os.Getenv(linuxParentDeathManifestEnvironment)},
				{Name: linuxParentDeathTargetEnvironment, Value: "1"},
			},
		},
		Deadline: 20 * time.Second, TerminationGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestLinuxParentDeathTarget(t *testing.T) {
	if os.Getenv(linuxParentDeathTargetEnvironment) != "1" {
		t.Skip("invoked only as an owned parent-death target")
	}
	signal.Ignore(syscall.SIGTERM)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	survivor := exec.Command(executable, "-test.run=^TestLinuxParentDeathSurvivor$", "-test.count=1")
	survivor.Env = []string{linuxParentDeathSurvivorEnvironment + "=1"}
	survivor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	ownerPID := os.Getppid()
	guardianPID, err := unix.Getsid(ownerPID)
	if err != nil {
		t.Fatalf("resolve guardian session identity: %v", err)
	}
	manifest := linuxParentDeathManifest{
		GuardianPID: guardianPID,
		OwnerPID:    ownerPID,
		RootPID:     os.Getpid(),
		SurvivorPID: survivor.Process.Pid,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(linuxParentDeathManifestEnvironment), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestLinuxParentDeathSurvivor(t *testing.T) {
	if os.Getenv(linuxParentDeathSurvivorEnvironment) != "1" {
		t.Skip("invoked only as an escaped parent-death descendant")
	}
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

type linuxParentDeathManifest struct {
	GuardianPID int `json:"guardian_pid"`
	OwnerPID    int `json:"owner_pid"`
	RootPID     int `json:"root_pid"`
	SurvivorPID int `json:"survivor_pid"`
}

func waitForLinuxParentDeathManifest(t *testing.T, path string) linuxParentDeathManifest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(path)
		if err == nil {
			var manifest linuxParentDeathManifest
			if json.Unmarshal(encoded, &manifest) == nil &&
				manifest.GuardianPID > 0 && manifest.OwnerPID > 0 &&
				manifest.RootPID > 0 && manifest.SurvivorPID > 0 {
				return manifest
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("parent-death target did not publish its process manifest")
	return linuxParentDeathManifest{}
}

func linuxParentDeathHelper(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv(linuxParentDeathHelperEnvironment); configured != "" {
		return configured
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner, err := BuildOwner(ctx, repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close parent-death owner fixture: %v", err)
		}
	})
	return owner.helperPath
}
