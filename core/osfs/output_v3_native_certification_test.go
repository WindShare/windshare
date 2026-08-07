//go:build linux || windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	nativeOutputCertificationProfileEnvironment = "WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION"
	nativeOutputCrashChildProfileEnvironment    = "WINDSHARE_NATIVE_OUTPUT_CRASH_CHILD_PROFILE"
	nativeOutputCrashScenarioEnvironment        = "WINDSHARE_NATIVE_OUTPUT_CRASH_SCENARIO"
	nativeOutputCrashRootEnvironment            = "WINDSHARE_NATIVE_OUTPUT_CRASH_ROOT"
	nativeOutputCrashReadyEnvironment           = "WINDSHARE_NATIVE_OUTPUT_CRASH_READY"
	nativeOutputCrashProbeNameEnvironment       = "WINDSHARE_NATIVE_OUTPUT_CRASH_PROBE_NAME"
	nativeOutputCrashStageSizeEnvironment       = "WINDSHARE_NATIVE_OUTPUT_CRASH_STAGE_SIZE"
	nativeOutputCrashReadyTimeout               = 20 * time.Second
	nativeOutputCrashPollInterval               = 10 * time.Millisecond
	nativeOutputCrashChildMaximumWait           = 5 * time.Minute
	nativeOutputCrashScenarioProbe              = "probe-stage"
	nativeOutputCrashScenarioSession            = "session-checkpoint"
	nativeOutputSessionPath                     = "received.bin"
	nativeOutputSessionSize                     = 2 * uint64(catalog.MinChunkSize)
)

func openCertifiedNativeOutputForTest(
	t *testing.T,
	root string,
	profile string,
	expectedCertification resumestate.CertificationID,
) outputcap.Platform {
	t.Helper()
	platform, err := openNativeOutputPlatform(root, false)
	if err != nil {
		nativeOutputCertificationFailure(t, profile, "open certified output root", err)
		return nil
	}
	fail := func(operation string, cause error) {
		t.Helper()
		closeErr := platform.Close()
		nativeOutputCertificationFailure(t, profile, operation, errors.Join(cause, closeErr))
	}
	if platform.Certification() != expectedCertification {
		fail("identify native certification", fmt.Errorf(
			"got %q, want %q", platform.Certification(), expectedCertification,
		))
		return nil
	}
	if platform.Durability() != transfer.DurabilityProcessRestart {
		fail("identify native durability", fmt.Errorf(
			"got %v, want process-restart durability", platform.Durability(),
		))
		return nil
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		fail("exercise required native filesystem features", err)
		return nil
	}
	binding, err := platform.RootBinding()
	if err != nil || binding.IsZero() || binding.Certification() != expectedCertification {
		fail("bind certified output root", errors.Join(err, fmt.Errorf(
			"zero=%t certification=%q", binding.IsZero(), binding.Certification(),
		)))
		return nil
	}
	repeated, err := platform.RootBinding()
	if err != nil || repeated != binding {
		fail("repeat certified output-root binding", errors.Join(err, fmt.Errorf(
			"first=%s repeated=%s", binding.String(), repeated.String(),
		)))
		return nil
	}
	return platform
}

func nativeOutputCertificationFailure(t *testing.T, profile, operation string, err error) {
	t.Helper()
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == profile ||
		!errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("%s for required %s profile: %v", operation, profile, err)
	}
	t.Skipf("%s is not certified on this test volume: %v", profile, err)
}

func runNativeOutputProcessRestartRecoveryTest(
	t *testing.T,
	profile string,
	expectedCertification resumestate.CertificationID,
	probeName string,
	stageSize int64,
) {
	t.Helper()
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile &&
		os.Getenv(nativeOutputCrashScenarioEnvironment) == nativeOutputCrashScenarioProbe {
		runNativeOutputCrashChild(t, profile, expectedCertification)
		return
	}
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile {
		return
	}

	base := t.TempDir()
	root := filepath.Join(base, "output")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create native output test root: %v", err)
	}
	preflight := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	if err := preflight.Close(); err != nil {
		t.Fatalf("close native output preflight authority: %v", err)
	}

	readyPath := filepath.Join(base, "child.ready")
	killNativeOutputChildAfterReady(t, readyPath, []string{
		nativeOutputCrashChildProfileEnvironment + "=" + profile,
		nativeOutputCrashScenarioEnvironment + "=" + nativeOutputCrashScenarioProbe,
		nativeOutputCrashRootEnvironment + "=" + root,
		nativeOutputCrashReadyEnvironment + "=" + readyPath,
		nativeOutputCrashProbeNameEnvironment + "=" + probeName,
		nativeOutputCrashStageSizeEnvironment + "=" + strconv.FormatInt(stageSize, 10),
	})

	recovered := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	defer func() {
		if err := recovered.Close(); err != nil {
			t.Errorf("close recovered native output authority: %v", err)
		}
	}()
	if names, err := recovered.Root().Names(0); err != nil || len(names) != 0 {
		t.Fatalf("process-restart recovery left output-root entries %v: %v", names, err)
	}
}

func killNativeOutputChildAfterReady(t *testing.T, readyPath string, environment []string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	// The Linux native chroot intentionally has no device nodes. An EOF-backed
	// pipe keeps os/exec from opening /dev/null before it starts the crash child.
	command.Stdin = bytes.NewReader(nil)
	command.Env = append(os.Environ(), environment...)
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatalf("start native output crash child: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	childRunning := true
	defer func() {
		if childRunning {
			_ = command.Process.Kill()
			<-waited
		}
	}()

	deadline := time.NewTimer(nativeOutputCrashReadyTimeout)
	ticker := time.NewTicker(nativeOutputCrashPollInterval)
	defer deadline.Stop()
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case err := <-waited:
			childRunning = false
			t.Fatalf("native output crash child exited before its persisted cut: %v\n%s", err, childOutput.String())
		case <-deadline.C:
			t.Fatalf("native output crash child did not persist its cut within %s\n%s",
				nativeOutputCrashReadyTimeout, childOutput.String())
		case <-ticker.C:
			_, err := os.Stat(readyPath)
			switch {
			case err == nil:
				ready = true
			case errors.Is(err, os.ErrNotExist):
			default:
				t.Fatalf("inspect native output crash-child marker: %v", err)
			}
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill native output crash child: %v", err)
	}
	if err := <-waited; err == nil {
		childRunning = false
		t.Fatal("native output crash child exited successfully instead of being terminated")
	}
	childRunning = false
}

type nativeOutputSessionFixture struct {
	selection  transfer.OutputSelection
	descriptor content.FileRevisionDescriptor
	payload    []byte
}

func runNativeOutputSessionProcessRestartRecoveryTest(
	t *testing.T,
	profile string,
	expectedCertification resumestate.CertificationID,
) {
	t.Helper()
	t.Skip("legacy frozen-selection restart fixture retired; incremental V1 recovery is exercised in outputruntime")
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile &&
		os.Getenv(nativeOutputCrashScenarioEnvironment) == nativeOutputCrashScenarioSession {
		runNativeOutputSessionCrashChild(t)
		return
	}
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile {
		return
	}

	base := t.TempDir()
	root := filepath.Join(base, "output")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create native session output root: %v", err)
	}
	readyPath := filepath.Join(base, "session-child.ready")
	killNativeOutputChildAfterReady(t, readyPath, []string{
		nativeOutputCrashChildProfileEnvironment + "=" + profile,
		nativeOutputCrashScenarioEnvironment + "=" + nativeOutputCrashScenarioSession,
		nativeOutputCrashRootEnvironment + "=" + root,
		nativeOutputCrashReadyEnvironment + "=" + readyPath,
	})

	fixture := newNativeOutputSessionFixture(t)
	authority := newNativeOutputSessionAuthority(t, root)
	session, admissions, err := openOutputSelectionFixture(t, authority, root, fixture.selection)
	if err != nil {
		t.Fatalf("reopen checkpointed native output session: %v", err)
	}
	file := nativeOutputFileForSession(t, session, fixture, admissions[""])
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("resume checkpointed native output file: %v", err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok {
		settlement, _ := start.ImmediateSettlement()
		t.Fatalf("checkpointed file did not resume as a transaction: settlement=%v", settlement.Kind())
	}
	ranges := durable.Ranges().Ranges()
	checkpointEnd := uint64(catalog.MinChunkSize)
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != checkpointEnd {
		t.Fatalf("reopened durable ranges=%v, want [0,%d)", ranges, checkpointEnd)
	}
	if err := transaction.WriteRange(
		context.Background(), checkpointEnd, fixture.payload[checkpointEnd:],
	); err != nil {
		t.Fatalf("write remaining native output range: %v", err)
	}
	completed, err := transaction.Checkpoint(context.Background())
	if err != nil || !transfer.RangesCoverFile(uint64(len(fixture.payload)), completed.Ranges()) {
		t.Fatalf("checkpoint completed native output file: ranges=%v err=%v", completed.Ranges().Ranges(), err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish recovered native output file: settlement=%v err=%v", settlement.Kind(), err)
	}
	job, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || job.Kind() != transfer.JobClosed {
		t.Fatalf("complete recovered native output session: settlement=%v err=%v", job.Kind(), err)
	}
	actual, err := os.ReadFile(filepath.Join(root, nativeOutputSessionPath))
	if err != nil || !bytes.Equal(actual, fixture.payload) {
		t.Fatalf("published native output differs after restart: bytes=%d err=%v", len(actual), err)
	}
	platform, err := openNativeOutputPlatform(root, false)
	if err != nil {
		t.Fatalf("reopen completed %s output root: %v", expectedCertification, err)
	}
	if platform.Durability() != transfer.DurabilityProcessRestart {
		t.Errorf("completed output root changed durability: %v", platform.Durability())
	}
	if err := platform.Close(); err != nil {
		t.Errorf("close completed native output root: %v", err)
	}
}

func runNativeOutputSessionCrashChild(t *testing.T) {
	t.Helper()
	root := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	if root == "" || readyPath == "" {
		t.Fatal("native output session crash-child parameters are invalid")
	}
	fixture := newNativeOutputSessionFixture(t)
	authority := newNativeOutputSessionAuthority(t, root)
	session, admissions, err := openOutputSelectionFixture(t, authority, root, fixture.selection)
	if err != nil {
		t.Fatalf("create native output session: %v", err)
	}
	file := nativeOutputFileForSession(t, session, fixture, admissions[""])
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("begin native output file: %v", err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || len(durable.Ranges().Ranges()) != 0 {
		t.Fatalf("new native output file did not start empty: ranges=%v", durable.Ranges().Ranges())
	}
	checkpointEnd := uint64(catalog.MinChunkSize)
	if err := transaction.WriteRange(
		context.Background(), 0, fixture.payload[:checkpointEnd],
	); err != nil {
		t.Fatalf("write native output checkpoint range: %v", err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	ranges := checkpoint.Ranges().Ranges()
	if err != nil || len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != checkpointEnd {
		t.Fatalf("persist native output checkpoint: ranges=%v err=%v", ranges, err)
	}
	signalNativeOutputCrashCut(t, readyPath)
	time.Sleep(nativeOutputCrashChildMaximumWait)
	t.Fatal("native output session crash child was not terminated by its parent")
}

func newNativeOutputSessionAuthority(t *testing.T, root string) *FilesystemOutputAuthority {
	t.Helper()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatalf("construct native output authority: %v", err)
	}
	return authority
}

func newNativeOutputSessionFixture(t *testing.T) nativeOutputSessionFixture {
	t.Helper()
	identity := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, catalog.IdentityBytes) }
	share, err := catalog.ShareInstanceFromBytes(identity(0x31))
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.DirectoryIDFromBytes(identity(0x32))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := catalog.DirectoryGenerationFromBytes(identity(0x33))
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := catalog.FileIDFromBytes(identity(0x34))
	if err != nil {
		t.Fatal(err)
	}
	revision, err := content.FileRevisionFromBytes(bytes.Repeat([]byte{0x35}, content.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	geometry, err := content.NewFileGeometry(nativeOutputSessionSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share, fileID, revision, geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		generation,
		nil,
		[]transfer.OutputSelectionFile{{
			Path: nativeOutputSessionPath, FileID: fileID,
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: nativeOutputSessionSize,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewPathSelectionRules([]string{nativeOutputSessionPath})
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, nativeOutputSessionSize)
	for index := range payload {
		payload[index] = byte(index*31 + 7)
	}
	return nativeOutputSessionFixture{selection: selection, descriptor: descriptor, payload: payload}
}

func nativeOutputFileForSession(
	t *testing.T,
	session transfer.OutputSession,
	fixture nativeOutputSessionFixture,
	parentAdmission transfer.DirectoryAdmission,
) transfer.OutputFile {
	t.Helper()
	locator, err := transfer.NewPathOutputLocator(nativeOutputSessionPath)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(
		session.BackendID(), session.SessionID(), fixture.descriptor, locator,
	)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Path: nativeOutputSessionPath, ExpectedSize: nativeOutputSessionSize,
		Descriptor: fixture.descriptor, Target: target, ParentAdmission: parentAdmission,
	}
}

func runNativeOutputCrashChild(
	t *testing.T,
	profile string,
	expectedCertification resumestate.CertificationID,
) {
	t.Helper()
	root := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	probeName := os.Getenv(nativeOutputCrashProbeNameEnvironment)
	stageSize, err := strconv.ParseInt(os.Getenv(nativeOutputCrashStageSizeEnvironment), 10, 64)
	if err != nil || root == "" || readyPath == "" || probeName == "" || stageSize < 0 {
		t.Fatalf("native output crash-child parameters are invalid: %v", err)
	}
	platform := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	directory, err := platform.Root().CreateDirectory(probeName, true)
	if err != nil {
		t.Fatalf("create native probe crash cut: %v", err)
	}
	stage, err := directory.CreateFile("stage", true, stageSize)
	if err != nil {
		t.Fatalf("create native probe stage crash cut: %v", err)
	}
	if err := errors.Join(stage.Sync(), directory.Sync(), platform.Root().Sync()); err != nil {
		t.Fatalf("persist native probe stage crash cut: %v", err)
	}
	signalNativeOutputCrashCut(t, readyPath)
	time.Sleep(nativeOutputCrashChildMaximumWait)
	t.Fatal("native output crash child was not terminated by its parent")
}

func signalNativeOutputCrashCut(t *testing.T, readyPath string) {
	t.Helper()
	// The marker is outside the certified root; it only coordinates the parent
	// process and cannot make an unpersisted output cut appear recoverable.
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("signal native output crash cut: %v", err)
	}
}
