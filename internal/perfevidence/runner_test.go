package perfevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func TestRunnerBuildsOnceAndUsesFreshProcessesForSamplesAndProfile(t *testing.T) {
	stage := t.TempDir()
	commands := &fixtureCommandRunner{benchmarkOutput: unitBenchmarkOutput()}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	evidence, err := (Runner{Commands: commands, RunID: "run"}).Measure(
		context.Background(), stage, workload, prepared, environment, 2, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if commands.builds != 1 || len(evidence.Samples) != 2 || evidence.Profile == nil {
		t.Fatalf("measurement = %+v, builds = %d", evidence, commands.builds)
	}
	if evidence.Samples[0].Command.ProcessID == evidence.Samples[1].Command.ProcessID {
		t.Fatal("benchmark samples reused a process")
	}
	if evidence.Profile.Binary != artifactFromBinary(evidence.Binary) || !OraclesPassed(evidence.Oracles) {
		t.Fatalf("profile or oracle evidence = %+v", evidence)
	}
	if len(evidence.Profile.Verification) != 2 || commands.pprofVerifications != 2 {
		t.Fatalf("profile verification evidence = %+v, calls = %d", evidence.Profile.Verification, commands.pprofVerifications)
	}
	for _, command := range commands.observed {
		if !command.ReplaceEnvironment || !reflect.DeepEqual(command.Environment, environment.Offline) {
			t.Fatalf("child command did not receive the exact sealed environment: %+v", command)
		}
	}
	for _, path := range []string{evidence.Profile.CPU.Path, evidence.Profile.Memory.Path, evidence.Binary.Path} {
		if _, err := os.Stat(filepath.Join(stage, filepath.FromSlash(path))); err != nil {
			t.Fatalf("artifact %s: %v", path, err)
		}
	}
}

func TestRunnerRejectsProfileThatControlledPprofCannotParse(t *testing.T) {
	stage := t.TempDir()
	commands := &fixtureCommandRunner{benchmarkOutput: unitBenchmarkOutput(), pprofFails: true}
	logger := &recordingEventLogger{}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	evidence, err := (Runner{Commands: commands, Logger: logger, RunID: "run"}).Measure(
		context.Background(), stage, workload, prepared, environment, 1, true,
	)
	if err == nil || !strings.Contains(err.Error(), "validate cpu pprof") {
		t.Fatalf("unparseable pprof was accepted: %v", err)
	}
	if evidence.Profile == nil || len(evidence.Profile.Verification) != 1 {
		t.Fatalf("failed pprof verification was not retained: %+v", evidence.Profile)
	}
	verification := evidence.Profile.Verification[0]
	if len(verification.Arguments) < 3 || verification.Arguments[0] != "tool" ||
		verification.Arguments[1] != "pprof" || verification.Arguments[2] != "-raw" {
		t.Fatalf("profile verification did not use controlled go tool pprof: %+v", verification)
	}
	if verification.Phase != EvidencePhaseProfileVerification ||
		verification.Outcome != EvidenceOutcomeFailed || verification.Error == "" {
		t.Fatalf("failed profile verification evidence = %+v", verification)
	}
	if terminals := logger.terminalEvents("unit-profile-cpu-verification"); len(terminals) != 1 || terminals[0].Outcome != "failed" {
		t.Fatalf("profile verification terminal events = %+v", terminals)
	}
}

func TestRunnerRejectsBinaryAndProfileReplacementDuringPprofParse(t *testing.T) {
	stage := t.TempDir()
	commands := &fixtureCommandRunner{benchmarkOutput: unitBenchmarkOutput()}
	commands.pprofMutation = func(command Command) error {
		binaryPath := command.Arguments[3]
		profileDir := filepath.Dir(command.Arguments[4])
		replacements := map[string][]byte{
			binaryPath:                                []byte("mutate-benchmark-binary"),
			filepath.Join(profileDir, "cpu.pprof"):    []byte("CPU-PROFILE"),
			filepath.Join(profileDir, "memory.pprof"): []byte("MEMORY-PROFILE"),
		}
		for path, content := range replacements {
			if err := os.WriteFile(path, content, 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	evidence, err := (Runner{Commands: commands, RunID: "run"}).Measure(
		context.Background(), stage, workload, prepared, environment, 1, true,
	)
	if err == nil {
		t.Fatalf("simultaneous binary/profile replacement survived pprof parsing: %v", err)
	}
	if evidence.Profile == nil || len(evidence.Profile.Verification) != 1 ||
		evidence.Profile.Verification[0].Outcome != EvidenceOutcomeFailed {
		t.Fatalf("mutated pprof verification was not terminally failed: %+v", evidence.Profile)
	}
}

func TestRunnerRetainsProfileVerificationWhenLogWriteFails(t *testing.T) {
	stage := t.TempDir()
	binaryRelative := filepath.ToSlash(filepath.Join("binaries", "unit.test"))
	profileRelative := filepath.ToSlash(filepath.Join("profiles", "unit", "cpu.pprof"))
	for relative, content := range map[string][]byte{
		binaryRelative:  []byte("stable-benchmark-binary"),
		profileRelative: []byte("CPU-PROFILE"),
	} {
		path := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := inspectArtifactIdentity(stage, binaryRelative)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := inspectArtifactIdentity(stage, profileRelative)
	if err != nil {
		t.Fatal(err)
	}
	blockedLog := filepath.Join(stage, "logs", "unit", "profile-cpu-verification.log")
	if err := os.MkdirAll(blockedLog, 0o700); err != nil {
		t.Fatal(err)
	}
	logger := &recordingEventLogger{}
	verification, err := (Runner{
		Commands: &fixtureCommandRunner{}, Logger: logger, RunID: "verification-log-failure",
	}).verifyProfile(
		context.Background(), t.TempDir(), stage,
		filepath.Join(stage, filepath.FromSlash(binaryRelative)), unitWorkload(),
		controlledGoEnvironment{GoExecutable: "go"}, binary, "cpu",
		filepath.Join(stage, filepath.FromSlash(profileRelative)), profile,
	)
	if err == nil || !strings.Contains(err.Error(), "write cpu profile verification log") ||
		verification.Phase != EvidencePhaseProfileVerification ||
		verification.Outcome != EvidenceOutcomeFailed || verification.Error == "" {
		t.Fatalf("profile verification log failure = %+v, err = %v", verification, err)
	}
	if terminals := logger.terminalEvents("unit-profile-cpu-verification"); len(terminals) != 1 || terminals[0].Outcome != "failed" {
		t.Fatalf("profile verification terminal events = %+v", terminals)
	}
}

func TestRunnerPreservesFailingSampleLog(t *testing.T) {
	stage := t.TempDir()
	commands := &fixtureCommandRunner{benchmarkOutput: []byte("not a benchmark\n")}
	logger := &recordingEventLogger{}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	evidence, err := (Runner{Commands: commands, Logger: logger, RunID: "failing-sample"}).Measure(
		context.Background(), stage, workload, prepared, environment, 1, false,
	)
	if err == nil || len(evidence.Samples) != 1 ||
		evidence.Samples[0].Command.Outcome != EvidenceOutcomeFailed ||
		len(evidence.Samples[0].Command.Artifacts) != 1 {
		t.Fatalf("measurement error = %v, evidence = %+v", err, evidence)
	}
	log, readErr := os.ReadFile(filepath.Join(stage, "logs", "unit", "sample-01.log"))
	if readErr != nil || string(log) != "not a benchmark\n" {
		t.Fatalf("failure log = %q, err = %v", log, readErr)
	}
	if got := logger.terminalEvents("unit-sample-01"); len(got) != 1 || got[0].Outcome != "failed" {
		t.Fatalf("sample terminal events = %+v", got)
	}
}

func TestRunnerEmitsOneFailedTerminalForEarlyBuildFailure(t *testing.T) {
	t.Parallel()
	stage := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(stage, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	logger := &recordingEventLogger{}
	evidence, err := (Runner{
		Commands: &fixtureCommandRunner{}, Logger: logger, RunID: "early-build",
	}).Measure(context.Background(), stage, workload, prepared, environment, 1, false)
	if err == nil || evidence.Build.Outcome != EvidenceOutcomeFailed || evidence.Build.Error == "" {
		t.Fatalf("early build evidence = %+v, err = %v", evidence.Build, err)
	}
	terminals := logger.terminalEvents(workload.ID)
	if len(terminals) != 1 || terminals[0].Milestone != "build-finished" || terminals[0].Outcome != "failed" {
		t.Fatalf("build terminal events = %+v", terminals)
	}
}

func TestRunnerRetainsProfilePhaseForEarlyFilesystemFailure(t *testing.T) {
	stageRoot := filepath.Join(t.TempDir(), "blocked-stage")
	if err := os.WriteFile(stageRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := &recordingEventLogger{}
	evidence, err := (Runner{
		Commands: &fixtureCommandRunner{}, Logger: logger, RunID: "early-profile",
	}).captureProfile(
		context.Background(), t.TempDir(), stageRoot, "unused-binary", unitWorkload(),
		controlledGoEnvironment{}, BinaryEvidence{}, nil,
	)
	if err == nil || evidence.Command.Phase != EvidencePhaseProfile ||
		evidence.Command.Outcome != EvidenceOutcomeFailed || evidence.Command.Error == "" {
		t.Fatalf("early profile evidence = %+v, err = %v", evidence.Command, err)
	}
	terminals := logger.terminalEvents("unit-profile")
	if len(terminals) != 1 || terminals[0].Milestone != "profile-finished" || terminals[0].Outcome != "failed" {
		t.Fatalf("profile terminal events = %+v", terminals)
	}
}

func TestRunnerReturnsTerminalLoggerFailure(t *testing.T) {
	t.Parallel()
	stage := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(stage, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload := unitWorkload()
	prepared, environment := unitBuildPlan(t, workload)
	logger := &recordingEventLogger{failMilestone: "build-finished"}
	_, err := (Runner{
		Commands: &fixtureCommandRunner{}, Logger: logger, RunID: "logger-failure",
	}).Measure(context.Background(), stage, workload, prepared, environment, 1, false)
	if err == nil || !strings.Contains(err.Error(), "event sink unavailable") {
		t.Fatalf("terminal logger failure was not observable: %v", err)
	}
}

func TestJSONLoggerReturnsWriterFailure(t *testing.T) {
	t.Parallel()
	logger := &JSONLogger{Writer: rejectingEventWriter{}}
	if err := logger.Log(Event{RunID: "writer-failure"}); err == nil ||
		!strings.Contains(err.Error(), "event sink unavailable") {
		t.Fatalf("JSON logger writer failure = %v", err)
	}
}

func TestApplicationPublishesSuccessfulAndFailedEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "evidence")
	commands := &fixtureCommandRunner{
		gitFiles:        []byte("source.txt\x00"),
		benchmarkOutput: readyBenchmarkOutput(),
	}
	application := Application{
		Commands:        commands,
		Now:             func() time.Time { return time.Unix(100, 0) },
		NewRunID:        func() (string, error) { return "successful-run", nil },
		Snapshots:       fixtureSnapshotPreparer(t),
		MutationDomains: passthroughMutationDomainFactory{},
	}
	publication, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: root, OutputRoot: output, SampleCount: 1, WorkloadIDs: []string{"ready-scaling"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublication(publication.Path, publication.EvidenceID); err != nil {
		t.Fatal(err)
	}

	failingCommands := &fixtureCommandRunner{gitFiles: []byte("source.txt\x00"), benchmarkOutput: []byte("bad\n")}
	failingApplication := Application{
		Commands:        failingCommands,
		Now:             func() time.Time { return time.Unix(200, 0) },
		NewRunID:        func() (string, error) { return "failed-run", nil },
		Snapshots:       fixtureSnapshotPreparer(t),
		MutationDomains: passthroughMutationDomainFactory{},
	}
	failedPublication, err := failingApplication.Run(context.Background(), RunConfig{
		RepositoryRoot: root, OutputRoot: output, SampleCount: 1, WorkloadIDs: []string{"ready-scaling"},
	})
	if err == nil || failedPublication.Path == "" {
		t.Fatalf("failed publication = %+v, err = %v", failedPublication, err)
	}
	payload, readErr := os.ReadFile(filepath.Join(failedPublication.Path, payloadName))
	if readErr != nil || !strings.Contains(string(payload), `"status":"failed"`) {
		t.Fatalf("failed payload = %s, err = %v", payload, readErr)
	}
}

func TestApplicationRejectsSnapshotMutationBeforePublication(t *testing.T) {
	repository := t.TempDir()
	output := filepath.Join(t.TempDir(), "evidence")
	base := fixtureSnapshotPreparer(t)
	tampered := SnapshotPreparerFunc(func(
		ctx context.Context,
		runner CommandRunner,
		repositoryRoot, artifactRoot, runtimeRoot string,
		workloads []Workload,
	) (PreparedSnapshot, error) {
		snapshot, err := base.Prepare(ctx, runner, repositoryRoot, artifactRoot, runtimeRoot, workloads)
		if err != nil {
			return PreparedSnapshot{}, err
		}
		snapshot.revalidator = snapshotRevalidatorFunc(func() error {
			return errors.New("snapshot bytes changed")
		})
		return snapshot, nil
	})
	application := Application{
		Commands:        &fixtureCommandRunner{benchmarkOutput: readyBenchmarkOutput()},
		NewRunID:        func() (string, error) { return "tampered-run", nil },
		Snapshots:       tampered,
		MutationDomains: passthroughMutationDomainFactory{},
	}
	publication, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository,
		OutputRoot:     output,
		SampleCount:    1,
		WorkloadIDs:    []string{"ready-scaling"},
	})
	if err == nil || publication.Path != "" || !strings.Contains(err.Error(), "revalidate sealed source snapshot") {
		t.Fatalf("tampered publication = %+v, err = %v", publication, err)
	}
	entries, readErr := os.ReadDir(output)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("tampered snapshot left publishable state: entries=%v, err=%v", entries, readErr)
	}
}

func TestApplicationRequiresMutationDomainBeforeCreatingPublicationState(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "evidence")
	publication, err := (Application{Commands: &fixtureCommandRunner{}}).Run(
		context.Background(),
		RunConfig{RepositoryRoot: t.TempDir(), OutputRoot: output, SampleCount: 1},
	)
	if err == nil || publication.Path != "" || !strings.Contains(err.Error(), "private mutation domain factory") {
		t.Fatalf("domainless application = %+v, err = %v", publication, err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("domainless application created publication state: %v", statErr)
	}
}

func TestMainWithoutMutationDomainCannotPublish(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "evidence")
	var stdout strings.Builder
	var stderr strings.Builder
	code := Main(context.Background(), []string{
		"-repository", t.TempDir(), "-output", output, "-samples", "1", "-workloads", "ready-scaling",
	}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "private mutation domain factory") {
		t.Fatalf("domainless Main exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("domainless Main created publication state: %v", statErr)
	}
}

func TestApplicationRejectsSampleCountAboveResourceBoundBeforeStaging(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "evidence")
	commands := &fixtureCommandRunner{}
	publication, err := (Application{
		Commands: commands, MutationDomains: passthroughMutationDomainFactory{},
	}).Run(context.Background(), RunConfig{
		RepositoryRoot: t.TempDir(), OutputRoot: output, SampleCount: MaximumSampleCount + 1,
	})
	if err == nil || publication.Path != "" || !strings.Contains(err.Error(), fmt.Sprint(MaximumSampleCount)) {
		t.Fatalf("oversized sample request = %+v, err = %v", publication, err)
	}
	if commands.calls != 0 {
		t.Fatalf("oversized sample request executed %d commands", commands.calls)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized sample request created stage state: %v", statErr)
	}
}

func TestHostAndCLIInventory(t *testing.T) {
	commands := &fixtureCommandRunner{}
	host := InspectHost(context.Background(), commands, t.TempDir())
	if host.OS == "" || host.OSVersion == "" || host.Architecture == "" || host.LogicalProcessors < 1 || len(host.Tools) != 4 {
		t.Fatalf("host metadata = %+v", host)
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "linux") && host.PhysicalMemoryBytes == 0 {
		t.Fatalf("native physical-memory probe failed: %+v", host)
	}
	var stdout strings.Builder
	var stderr strings.Builder
	if code := Main(context.Background(), []string{"-list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit = %d, stderr = %s", code, stderr.String())
	}
	listed := strings.Fields(stdout.String())
	if len(listed) != 7 || !reflect.DeepEqual(listed, WorkloadIDs()) {
		t.Fatalf("workload list = %v, want all maintained workloads %v", listed, WorkloadIDs())
	}
	if code := Main(context.Background(), []string{"unexpected"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unexpected positional exit = %d", code)
	}
	if _, err := profileSet([]string{"missing"}, []Workload{unitWorkload()}); err == nil {
		t.Fatal("unknown profile workload was accepted")
	}
	profiles, err := profileSet([]string{"all"}, []Workload{unitWorkload()})
	if err != nil || !profiles["unit"] {
		t.Fatalf("all profiles = %+v, err = %v", profiles, err)
	}
}

func TestCLIReportsPublicOutputFailures(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	if code := Main(context.Background(), []string{"-list"}, rejectingEventWriter{}, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "write workload list") {
		t.Fatalf("list output failure: code=%d stderr=%q", code, stderr.String())
	}
	publication := Publication{EvidenceID: strings.Repeat("a", 64), Path: "evidence/path"}
	if err := writePublicationResult(rejectingEventWriter{}, publication); err == nil ||
		!strings.Contains(err.Error(), "event sink unavailable") {
		t.Fatalf("publication result output failure = %v", err)
	}
}

func TestProcessRunnerReportsSuccessAndLaunchFailure(t *testing.T) {
	runner := ProcessRunner{}
	result, err := runner.Run(context.Background(), Command{Executable: "go", Arguments: []string{"version"}})
	if err != nil || result.ExitCode != 0 || result.ProcessID <= 0 || len(result.Output) == 0 {
		t.Fatalf("go version = %+v, err = %v", result, err)
	}
	result, err = runner.Run(context.Background(), Command{Executable: filepath.Join(t.TempDir(), "missing-command")})
	if err == nil || result.ExitCode != -1 || result.ProcessID != 0 {
		t.Fatalf("missing command = %+v, err = %v", result, err)
	}
}

func TestCommandFailureAndEventVocabularyRemainUnambiguous(t *testing.T) {
	exitOnly := commandFailure("sample", CommandResult{ExitCode: 9, Output: []byte("rejected")}, nil)
	if got := exitOnly.Error(); got != "sample exited with 9: rejected" || strings.Contains(got, "%!w") {
		t.Fatalf("exit-only diagnostic = %q", got)
	}
	var output strings.Builder
	logger := &JSONLogger{Writer: &output}
	logger.Log(Event{
		RunID: "run", OperationID: "ready-scaling-sample-01", Scenario: "ready-scaling",
		Component: "perfevidence", Milestone: "sample-finished", Outcome: "succeeded",
	})
	encoded := output.String()
	for _, field := range []string{`"run_id"`, `"operation_id"`, `"scenario"`, `"component"`, `"milestone"`, `"outcome"`} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("event %s omitted %s", encoded, field)
		}
	}
	if strings.Contains(encoded, `"runId"`) || workloadScenario("ready-scaling-profile") != "ready-scaling" ||
		workloadScenario("ready-scaling-profile-cpu-verification") != "ready-scaling" {
		t.Fatalf("event vocabulary or scenario = %s", encoded)
	}
	if samplingPolicy(5).Classification != "ad-hoc" || samplingPolicy(20).Classification != "baseline-sized" {
		t.Fatal("sampling classification is ambiguous")
	}
	assessment := assessBaseline(
		5,
		SnapshotIdentity{Git: SourceIdentity{WorktreeDirty: true}},
		HostMetadata{RequiredErrors: []string{"missing"}},
		1, 7, fmt.Errorf("failed"),
	)
	if assessment.Eligible || len(assessment.Reasons) != 7 {
		t.Fatalf("baseline assessment = %+v", assessment)
	}
	if !assessBaseline(
		20,
		SnapshotIdentity{SHA256: strings.Repeat("a", 64), CompiledInputsMatchCommit: true},
		HostMetadata{}, 7, 7, nil,
	).Eligible {
		t.Fatal("complete stable baseline-sized evidence was rejected")
	}
}

func unitWorkload() Workload {
	return Workload{
		ID: "unit", ModuleDir: ".", Package: "./unit", Benchmark: "BenchmarkUnit", BenchTime: "1x",
		Contracts: []BenchmarkContract{{
			Name: "BenchmarkUnit/case=1", RequiredMetrics: []string{"ns/op", "B/op", "allocs/op", "objects/op"},
		}},
		HardOracles: []MetricOracle{{ID: "object", Metric: "objects/op", Comparison: Equal, Limit: 1}},
	}
}

func unitBenchmarkOutput() []byte {
	return []byte("BenchmarkUnit/case=1-8 1 10 ns/op 20 B/op 2 allocs/op 1 objects/op\nPASS\n")
}

func readyBenchmarkOutput() []byte {
	var output strings.Builder
	for _, descendants := range []int{0, 1_000, 10_000, 100_000, 1_000_000} {
		_, _ = fmt.Fprintf(
			&output,
			"BenchmarkReadyScaling/descendants=%07d-8 1 10 ns/op 20 B/op 2 allocs/op %d virtual-descendants 0 descendant-fs-ops/op 100 registration-material-bytes/op 50 descriptor-bytes/op\n",
			descendants, descendants,
		)
	}
	return []byte(output.String())
}

func unitBuildPlan(t *testing.T, workload Workload) (PreparedWorkload, controlledGoEnvironment) {
	t.Helper()
	moduleRoot := t.TempDir()
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"Replace":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return PreparedWorkload{
			ModuleRoot: moduleRoot, Package: workload.Package, OverlayPath: overlay,
			Graph: BuildGraphIdentity{ClosureSHA256: strings.Repeat("b", 64)},
		}, controlledGoEnvironment{
			GoExecutable: "go",
			Offline:      []string{"GOENV=off", "GOPROXY=off", "GOSUMDB=off"},
		}
}

func fixtureSnapshotPreparer(t *testing.T) SnapshotPreparer {
	t.Helper()
	return SnapshotPreparerFunc(func(
		_ context.Context,
		_ CommandRunner,
		repositoryRoot, artifactRoot, _ string,
		workloads []Workload,
	) (PreparedSnapshot, error) {
		plans := make(map[string]PreparedWorkload, len(workloads))
		for _, workload := range workloads {
			overlay := filepath.Join(artifactRoot, "overlays", workload.ID, "overlay.json")
			if err := writeExclusive(overlay, []byte(`{"Replace":{}}`)); err != nil {
				return PreparedSnapshot{}, err
			}
			plans[workload.ID] = PreparedWorkload{
				ModuleRoot: repositoryRoot, Package: workload.Package, OverlayPath: overlay,
				Graph: BuildGraphIdentity{ClosureSHA256: strings.Repeat("c", 64)},
			}
		}
		environment := []EnvironmentVariable{
			{Name: "CGO_ENABLED", Value: "0"}, {Name: "GOCACHE", Value: "cache"},
			{Name: "GOENV", Value: "off"}, {Name: "GOEXPERIMENT", Value: ""},
			{Name: "GOFLAGS", Value: ""}, {Name: "GOMODCACHE", Value: "modules"},
			{Name: "GOOS", Value: runtime.GOOS}, {Name: "GOARCH", Value: runtime.GOARCH},
			{Name: "GOPROXY", Value: "off"}, {Name: "GOSUMDB", Value: "off"},
			{Name: "GOTOOLCHAIN", Value: "local"}, {Name: "GOWORK", Value: "workspace"},
			{Name: "TEMP", Value: "temporary"},
		}
		identity := SnapshotIdentity{
			SHA256: strings.Repeat("d", 64), CompiledInputsMatchCommit: true,
			BuildEnvironment: environment,
			Toolchain: ToolchainIdentity{
				ExecutableSHA256: strings.Repeat("e", 64),
				Version:          "go version fixture", GoVersion: "go1.fixture",
				Tools: []ToolBinaryIdentity{{Name: "compile", Bytes: 1, SHA256: strings.Repeat("f", 64)}},
			},
			Diagnostics: SnapshotDiagnostics{
				ProcessEnvironment: environment, EffectiveGoEnv: environment,
				Toolchain: ToolchainDiagnostics{ExecutablePath: "go", GoRoot: "goroot", GoToolDir: "gotooldir"},
			},
		}
		authority := testConsumptionAuthority{}
		return PreparedSnapshot{
			Root:        filepath.Join(artifactRoot, snapshotDirectoryName),
			Environment: controlledGoEnvironment{GoExecutable: "go", Authority: authority},
			Identity:    identity, Workloads: plans,
			revalidator: snapshotRevalidatorFunc(func() error { return nil }),
			authority:   authority,
		}, nil
	})
}

type testConsumptionAuthority struct{}

func (testConsumptionAuthority) Verify() error { return nil }
func (testConsumptionAuthority) VerifyProcessStart(protocol.StartEvidence, string) (bool, error) {
	return true, nil
}
func (testConsumptionAuthority) Close() error { return nil }

type fixtureCommandRunner struct {
	mu                 sync.Mutex
	calls              int
	builds             int
	gitCaptures        int
	gitFiles           []byte
	benchmarkOutput    []byte
	pprofFails         bool
	pprofMutation      func(Command) error
	pprofVerifications int
	observed           []Command
}

type recordingEventLogger struct {
	mu            sync.Mutex
	events        []Event
	failMilestone string
}

func (logger *recordingEventLogger) Log(event Event) error {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if event.Milestone == logger.failMilestone {
		return errors.New("event sink unavailable")
	}
	logger.events = append(logger.events, event)
	return nil
}

func (logger *recordingEventLogger) terminalEvents(operationID string) []Event {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	var terminals []Event
	for _, event := range logger.events {
		if event.OperationID == operationID && strings.HasSuffix(event.Milestone, "-finished") {
			terminals = append(terminals, event)
		}
	}
	return terminals
}

type rejectingEventWriter struct{}

func (rejectingEventWriter) Write([]byte) (int, error) {
	return 0, errors.New("event sink unavailable")
}

func (runner *fixtureCommandRunner) withMutationDomain(MutationDomain) CommandRunner {
	return runner
}

func (runner *fixtureCommandRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls++
	recorded := command
	recorded.Arguments = append([]string(nil), command.Arguments...)
	recorded.Environment = append([]string(nil), command.Environment...)
	runner.observed = append(runner.observed, recorded)
	started := time.Unix(int64(runner.calls), 0)
	result := CommandResult{
		ProcessID: 1000 + runner.calls, ExitCode: 0,
		StartedAt: started, FinishedAt: started.Add(time.Millisecond),
	}
	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(command.Executable)), ".exe")
	switch executable {
	case "git":
		operation := command.Arguments[2]
		switch operation {
		case "rev-parse":
			result.Output = []byte(strings.Repeat("a", 40) + "\n")
		case "status":
			runner.gitCaptures++
		case "ls-files":
			result.Output = runner.gitFiles
		default:
			return CommandResult{}, fmt.Errorf("unexpected git operation %s", operation)
		}
	case "go":
		if slicesContain(command.Arguments, "test") && slicesContain(command.Arguments, "-c") {
			runner.builds++
			for index, argument := range command.Arguments {
				if argument == "-o" {
					if err := os.WriteFile(command.Arguments[index+1], []byte("stable-benchmark-binary"), 0o700); err != nil {
						return result, err
					}
				}
			}
		} else if len(command.Arguments) >= 2 && command.Arguments[0] == "tool" && command.Arguments[1] == "buildid" {
			result.Output = []byte("stable-build-id\n")
		} else if len(command.Arguments) >= 2 && command.Arguments[0] == "tool" && command.Arguments[1] == "pprof" {
			runner.pprofVerifications++
			if runner.pprofMutation != nil {
				mutation := runner.pprofMutation
				runner.pprofMutation = nil
				if err := mutation(command); err != nil {
					return result, err
				}
			}
			result.Output = []byte("fixture pprof decoded\n")
			if runner.pprofFails {
				result.ExitCode = 2
				result.Output = []byte("malformed profile\n")
			}
		} else if len(command.Arguments) >= 2 && command.Arguments[0] == "version" && command.Arguments[1] == "-m" {
			result.Output = []byte(command.Arguments[2] + ": go1.fixture\n")
		} else {
			result.Output = []byte("go version fixture\n")
		}
	case "node", "pnpm":
		result.Output = []byte("fixture-version\n")
	default:
		result.Output = runner.benchmarkOutput
		for _, argument := range command.Arguments {
			if path, found := strings.CutPrefix(argument, "-test.cpuprofile="); found {
				if err := os.WriteFile(path, []byte("cpu-profile"), 0o600); err != nil {
					return result, err
				}
			}
			if path, found := strings.CutPrefix(argument, "-test.memprofile="); found {
				if err := os.WriteFile(path, []byte("memory-profile"), 0o600); err != nil {
					return result, err
				}
			}
		}
	}
	return result, nil
}

func slicesContain(values []string, wanted string) bool {
	return slices.Contains(values, wanted)
}
