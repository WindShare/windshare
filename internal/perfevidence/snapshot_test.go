package perfevidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPreparedSnapshotBuildsOnlySemanticPerformanceTests(t *testing.T) {
	if testing.Short() {
		t.Skip("requires repeated real Go graph inventories and benchmark builds")
	}
	repository := newSnapshotFixtureRepository(t)
	workload := snapshotFixtureWorkload()
	artifactRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	prepared, err := prepareSnapshot(
		context.Background(), ProcessRunner{}, repository, artifactRoot, runtimeRoot, []Workload{workload},
		passthroughMutationDomainFactory{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Errorf("close prepared snapshot: %v", err)
		}
	})
	if !prepared.Identity.CompiledInputsMatchCommit || len(prepared.Identity.UncommittedInputs) != 0 {
		t.Fatalf("committed snapshot = %+v", prepared.Identity.UncommittedInputs)
	}
	graph := prepared.Workloads[workload.ID].Graph
	if len(graph.PerformanceTests) != 1 || graph.PerformanceTests[0] != "sample_perfevidence_test.go" {
		t.Fatalf("performance tests = %v", graph.PerformanceTests)
	}
	if len(graph.SuppressedTests) != 1 || graph.SuppressedTests[0] != "correctness_test.go" {
		t.Fatalf("suppressed tests = %v", graph.SuppressedTests)
	}
	for _, dependency := range graph.DependencyPackages {
		if strings.Contains(dependency, "testfixture") {
			t.Fatalf("correctness fixture entered performance graph: %s", dependency)
		}
	}
	stubbed, err := os.ReadFile(filepath.Join(prepared.Root, "workspace", "sample", "correctness_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stubbed) != "package sample\n" {
		t.Fatalf("snapshot correctness file was not stubbed: %q", stubbed)
	}
	measurementCommands, err := runnerWithMutationDomain(ProcessRunner{}, prepared.domain)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := (Runner{Commands: measurementCommands, RunID: "snapshot-test"}).Measure(
		context.Background(), artifactRoot, workload, prepared.Workloads[workload.ID], prepared.Environment, 1, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Samples) != 1 || !OraclesPassed(evidence.Oracles) || evidence.Binary.BuildGraphSHA256 != graph.ClosureSHA256 {
		t.Fatalf("snapshot measurement = %+v", evidence)
	}
	if evidence.Profile == nil || len(evidence.Profile.Verification) != 2 {
		t.Fatalf("real pprof validation evidence = %+v", evidence.Profile)
	}
	if err := prepared.Revalidate(); err != nil {
		t.Fatalf("unchanged snapshot failed final-byte validation: %v", err)
	}
	exactEnvironment, err := processEnvironmentEvidence(prepared.Environment.Offline)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Identity.Diagnostics.ProcessEnvironment, exactEnvironment) {
		t.Fatalf(
			"diagnostic child environment differs from executed environment:\nrecorded=%+v\nexecuted=%+v",
			prepared.Identity.Diagnostics.ProcessEnvironment, exactEnvironment,
		)
	}
	stubPath := filepath.Join(artifactRoot, filepath.FromSlash(graph.OverlayMappings[0].StubPath))
	assertSnapshotMutationRejectedOrDetected(
		t, &prepared, stubPath, func(original []byte) []byte {
			return append(append([]byte(nil), original...), []byte("// mutated after measurement\n")...)
		},
	)
	productPath := filepath.Join(prepared.Root, "workspace", "sample", "product.go")
	assertSnapshotMutationRejectedOrDetected(
		t, &prepared, productPath, func([]byte) []byte {
			return []byte("package sample\n\nfunc ProductValue() int { return 2 }\n")
		},
	)

	ignored := filepath.Join(repository, "sample", "generated.go")
	if err := os.WriteFile(ignored, []byte("package sample\n\nconst ignoredCompiledInput = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignoredSnapshot, err := prepareSnapshot(
		context.Background(), ProcessRunner{}, repository, t.TempDir(), t.TempDir(), []Workload{workload},
		passthroughMutationDomainFactory{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ignoredSnapshot.Close(); err != nil {
			t.Errorf("close ignored snapshot: %v", err)
		}
	})
	if ignoredSnapshot.Identity.CompiledInputsMatchCommit || !containsString(ignoredSnapshot.Identity.UncommittedInputs, "sample/generated.go") {
		t.Fatalf("ignored compiled input was baseline-eligible: %+v", ignoredSnapshot.Identity.UncommittedInputs)
	}
}

func assertSnapshotMutationRejectedOrDetected(
	t *testing.T,
	prepared *PreparedSnapshot,
	path string,
	mutate func([]byte) []byte,
) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		if revalidateErr := prepared.Revalidate(); revalidateErr != nil {
			t.Fatalf("authority denied mutation setup but unchanged snapshot failed revalidation: %v", revalidateErr)
		}
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		if revalidateErr := prepared.Revalidate(); revalidateErr != nil {
			t.Fatalf("authority denied mutation but unchanged snapshot failed revalidation: %v", revalidateErr)
		}
		return
	}
	if err := os.WriteFile(path, mutate(original), 0o600); err != nil {
		if restoreErr := os.Chmod(path, info.Mode().Perm()); restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if revalidateErr := prepared.Revalidate(); revalidateErr != nil {
			t.Fatalf("authority denied mutation but unchanged snapshot failed revalidation: %v", revalidateErr)
		}
		return
	}
	if err := prepared.Revalidate(); err == nil {
		t.Fatal("post-measurement snapshot mutation remained publishable")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Revalidate(); err != nil {
		t.Fatalf("restored snapshot failed final-byte validation: %v", err)
	}
}

func TestSnapshotAndBinaryIdentityIgnoreCheckoutAndRuntimeLocations(t *testing.T) {
	if testing.Short() {
		t.Skip("requires two cloned checkouts and real Go benchmark builds")
	}
	source := newSnapshotFixtureRepository(t)
	checkoutRoot := t.TempDir()
	checkouts := []string{
		filepath.Join(checkoutRoot, "checkout-a"),
		filepath.Join(checkoutRoot, "different", "absolute", "checkout-b"),
	}
	for _, checkout := range checkouts {
		cloneSnapshotFixture(t, source, checkout)
	}

	type outcome struct {
		snapshot PreparedSnapshot
		binary   BinaryEvidence
		artifact string
		runtime  string
	}
	workload := snapshotFixtureWorkload()
	outcomes := make([]outcome, 0, len(checkouts))
	for index, checkout := range checkouts {
		artifactRoot := filepath.Join(t.TempDir(), "stage", string(rune('a'+index)))
		runtimeRoot := filepath.Join(t.TempDir(), "runtime", string(rune('a'+index)))
		prepared, err := prepareSnapshot(
			context.Background(), ProcessRunner{}, checkout, artifactRoot, runtimeRoot, []Workload{workload},
			passthroughMutationDomainFactory{},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := prepared.Close(); err != nil {
				t.Errorf("close prepared snapshot: %v", err)
			}
		})
		measurementCommands, err := runnerWithMutationDomain(ProcessRunner{}, prepared.domain)
		if err != nil {
			t.Fatal(err)
		}
		measured, err := (Runner{Commands: measurementCommands, RunID: "location-stability"}).Measure(
			context.Background(), artifactRoot, workload, prepared.Workloads[workload.ID], prepared.Environment, 1, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, outcome{
			snapshot: prepared, binary: measured.Binary, artifact: artifactRoot, runtime: runtimeRoot,
		})
	}

	left, right := outcomes[0], outcomes[1]
	if left.snapshot.Identity.SHA256 != right.snapshot.Identity.SHA256 {
		t.Fatalf("source snapshot identity depends on location: %s != %s", left.snapshot.Identity.SHA256, right.snapshot.Identity.SHA256)
	}
	leftGraph := left.snapshot.Workloads[workload.ID].Graph
	rightGraph := right.snapshot.Workloads[workload.ID].Graph
	if leftGraph.ClosureSHA256 != rightGraph.ClosureSHA256 || leftGraph.OverlaySHA256 != rightGraph.OverlaySHA256 {
		t.Fatalf("build closure depends on location: %+v != %+v", leftGraph, rightGraph)
	}
	if left.binary.SHA256 != right.binary.SHA256 || left.binary.GoBuildID != right.binary.GoBuildID {
		t.Fatalf("benchmark binary depends on location: %+v != %+v", left.binary, right.binary)
	}
	if left.binary.GoVersionMetadata != right.binary.GoVersionMetadata {
		t.Fatalf("canonical Go metadata depends on location:\n%s\n!=\n%s", left.binary.GoVersionMetadata, right.binary.GoVersionMetadata)
	}
	if !reflect.DeepEqual(left.snapshot.Identity.BuildEnvironment, right.snapshot.Identity.BuildEnvironment) {
		t.Fatalf("comparable build environment depends on location: %+v != %+v", left.snapshot.Identity.BuildEnvironment, right.snapshot.Identity.BuildEnvironment)
	}
	if reflect.DeepEqual(
		left.snapshot.Identity.Diagnostics.ProcessEnvironment,
		right.snapshot.Identity.Diagnostics.ProcessEnvironment,
	) {
		t.Fatal("test did not exercise different diagnostic runtime locations")
	}
	if reflect.DeepEqual(
		left.snapshot.Identity.Diagnostics.OverlayFiles,
		right.snapshot.Identity.Diagnostics.OverlayFiles,
	) {
		t.Fatal("test did not exercise location-dependent overlay file bytes")
	}
	for _, result := range outcomes {
		for _, transient := range []string{
			result.artifact,
			result.runtime,
			result.snapshot.Root,
			result.snapshot.Workloads[workload.ID].OverlayPath,
		} {
			if containsCanonicalPath(result.binary.GoVersionMetadata, transient) {
				t.Fatalf("Go version metadata leaked transient path %s: %s", transient, result.binary.GoVersionMetadata)
			}
		}
	}
}

func TestPerformanceDependencyDeltaRejectsRenamedTestOnlyRepositoryPackage(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	targetImport := "example.com/project/sample"
	productionImport := "example.com/project/production"
	renamedFixtureImport := "example.com/project/internal/scenario"
	packages := []goListPackage{
		{
			Dir: filepath.Join(workspace, "sample"), ImportPath: targetImport,
			Deps: []string{productionImport, "fmt"},
		},
		{Dir: filepath.Join(workspace, "production"), ImportPath: productionImport},
		{Dir: filepath.Join(workspace, "internal", "scenario"), ImportPath: renamedFixtureImport},
		{Dir: filepath.Join(runtime.GOROOT(), "src", "fmt"), ImportPath: "fmt", Standard: true},
	}
	context := inventoryContext{
		WorkspaceRoot: workspace,
		Overlay:       workloadOverlay{PackageImport: targetImport},
	}
	if err := validatePerformanceDependencyDelta(packages[:2], context); err != nil {
		t.Fatalf("legitimate production closure was rejected: %v", err)
	}
	if err := validatePerformanceDependencyDelta(packages, context); err == nil ||
		!strings.Contains(err.Error(), renamedFixtureImport) {
		t.Fatalf("renamed test-only repository package was accepted: %v", err)
	}
}

func TestPerformanceDependencyDeltaAcceptsExplicitHarnessClosure(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	targetImport := "example.com/project/sample"
	harnessImport := "example.com/project/benchmarkfixture"
	harnessDependency := "example.com/project/benchmarkfixture/data"
	context := inventoryContext{
		WorkspaceRoot: workspace,
		Overlay: workloadOverlay{
			PackageImport: targetImport, BenchmarkHarnessPackages: []string{harnessImport},
		},
	}
	packages := []goListPackage{
		{Dir: filepath.Join(workspace, "sample"), ImportPath: targetImport},
		{
			Dir: filepath.Join(workspace, "benchmarkfixture"), ImportPath: harnessImport,
			Deps: []string{harnessDependency},
		},
		{Dir: filepath.Join(workspace, "benchmarkfixture", "data"), ImportPath: harnessDependency},
	}
	if err := validatePerformanceDependencyDelta(packages, context); err != nil {
		t.Fatalf("explicit benchmark harness closure was rejected: %v", err)
	}
}

func TestControlledEnvironmentRemovesHostileGoConfiguration(t *testing.T) {
	t.Setenv("GOFLAGS", "-overlay=hostile.json")
	t.Setenv("GOWORK", "hostile.work")
	t.Setenv("GOEXPERIMENT", "hostile")
	actual := exactProcessEnvironment(map[string]string{
		"GOENV": "off", "GOFLAGS": "", "GOWORK": "sealed.work", "GOEXPERIMENT": "",
	})
	joined := strings.Join(actual, "\n")
	for _, expected := range []string{"GOENV=off", "GOFLAGS=", "GOWORK=sealed.work", "GOEXPERIMENT="} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("controlled environment omitted %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "hostile") {
		t.Fatalf("host configuration leaked into controlled environment: %s", joined)
	}
}

func TestToolchainInputsCoverHeadersAndExternalToolDirectory(t *testing.T) {
	goRoot := t.TempDir()
	includeRoot := filepath.Join(goRoot, filepath.FromSlash(goAssemblyIncludeRelativePath))
	goToolDir := filepath.Join(t.TempDir(), "custom-tools")
	for _, directory := range []string{includeRoot, goToolDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	headerPath := filepath.Join(includeRoot, "textflag.h")
	toolPath := filepath.Join(goToolDir, "compile")
	goExecutable := filepath.Join(t.TempDir(), "go")
	for path, content := range map[string]string{
		headerPath:   "header-a",
		toolPath:     "compiler-a",
		goExecutable: "driver-a",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := identifyToolchainInputsForRoots(
		goRoot, goToolDir, goAssemblyIncludeRelativePath,
	)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{
		string(ToolchainInputGoRoot) + ":pkg/include/textflag.h": false,
		string(ToolchainInputGoToolDir) + ":compile":             false,
	}
	for _, input := range inputs {
		key := string(input.Root) + ":" + input.Path
		if _, found := wanted[key]; found {
			wanted[key] = true
		}
	}
	for input, found := range wanted {
		if !found {
			t.Fatalf("toolchain inventory omitted %s: %+v", input, inputs)
		}
	}
	executableSHA, err := hashFile(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	identity := SnapshotIdentity{Toolchain: ToolchainIdentity{
		ExecutableSHA256: executableSHA,
		BuildInputs:      inputs,
	}}
	identity.SHA256, err = snapshotIdentitySHA(identity)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := newSnapshotValidationPlan(t.TempDir(), nil, identity, controlledGoEnvironment{
		ToolchainLocations: ToolchainDiagnostics{
			ExecutablePath: goExecutable, GoRoot: goRoot, GoToolDir: goToolDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Revalidate(); err != nil {
		t.Fatalf("unchanged toolchain closure failed revalidation: %v", err)
	}
	if err := os.WriteFile(headerPath, []byte("header-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plan.Revalidate(); err == nil || !strings.Contains(err.Error(), "pkg/include/textflag.h") {
		t.Fatalf("mutated assembly header remained publishable: %v", err)
	}
	if err := os.WriteFile(headerPath, []byte("header-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolPath, []byte("compiler-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plan.Revalidate(); err == nil || !strings.Contains(err.Error(), "gotooldir/compile") {
		t.Fatalf("mutated external GOTOOLDIR input remained publishable: %v", err)
	}
}

func TestPathIdentityKeepsCaseDistinctInputsSeparate(t *testing.T) {
	root := t.TempDir()
	upperRoot := filepath.Join(root, "Case")
	lowerRoot := filepath.Join(root, "case")
	for _, directory := range []string{upperRoot, lowerRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			if os.IsExist(err) {
				t.Skip("test filesystem does not support case-distinct directory entries")
			}
			t.Fatal(err)
		}
	}
	upper := filepath.Join(upperRoot, "input.go")
	lower := filepath.Join(lowerRoot, "input.go")
	for _, path := range []string{upper, lower} {
		if err := os.WriteFile(path, []byte("same bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("test filesystem aliases case-distinct path spellings")
	}
	if canonicalPath(upper) == canonicalPath(lower) {
		t.Fatal("OS path identity collapsed two distinct files")
	}
	if _, inside := relativeWithin(upperRoot, lower); inside {
		t.Fatal("case-distinct sibling was classified inside the wrong authority")
	}
	targets := make(map[string]snapshotValidationTarget)
	for _, path := range []string{upper, lower} {
		sha, hashErr := hashFile(path)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		if err := addSnapshotValidationTarget(targets, snapshotValidationTarget{
			LogicalPath: path, PhysicalPath: path, Bytes: upperInfo.Size(), SHA256: sha,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("case-distinct validation targets collapsed: %+v", targets)
	}
}

func TestEvidenceOutputInsideRepositoryRequiresIgnoreAuthority(t *testing.T) {
	repository := newSnapshotFixtureRepository(t)
	outputRoot := filepath.Join(repository, "evidence")
	application := Application{
		Commands:        ProcessRunner{},
		NewRunID:        func() (string, error) { return "ignore-authority", nil },
		MutationDomains: passthroughMutationDomainFactory{},
	}
	publication, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository,
		OutputRoot:     outputRoot,
		SampleCount:    1,
		WorkloadIDs:    []string{"ready-scaling"},
	})
	if err == nil || publication.Path != "" || !strings.Contains(err.Error(), "must be Git-ignored") {
		t.Fatalf("unignored repository output = %+v, err = %v", publication, err)
	}
	entries, statErr := os.ReadDir(outputRoot)
	if statErr != nil || len(entries) != 0 {
		t.Fatalf("rejected output authority contains pre-validation children: entries=%v err=%v", entries, statErr)
	}
	ignorePath := filepath.Join(repository, ".gitignore")
	ignore, err := os.ReadFile(ignorePath)
	if err != nil {
		t.Fatal(err)
	}
	leafRunID := "leaf-only"
	leafPatterns := []byte(strings.Join([]string{
		"/evidence/.staging-" + leafRunID + "/source-snapshot/workspace/go.mod",
		"/evidence/.runtime-" + leafRunID + "/" + stageOwnerName,
		"/evidence/" + strings.Repeat("a", 64) + "/" + payloadName,
	}, "\n") + "\n")
	if err := os.WriteFile(ignorePath, append(ignore, leafPatterns...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{
		"evidence/.staging-" + leafRunID + "/source-snapshot/workspace/go.mod",
		"evidence/.runtime-" + leafRunID + "/" + stageOwnerName,
		"evidence/" + strings.Repeat("a", 64) + "/" + payloadName,
	} {
		result, runErr := (ProcessRunner{}).Run(context.Background(), Command{
			Executable: "git",
			Arguments:  []string{"-C", repository, "check-ignore", "--quiet", "--no-index", "--", probe},
		})
		if runErr != nil || result.ExitCode != 0 {
			t.Fatalf("adversarial leaf pattern did not ignore %s: exit=%d err=%v", probe, result.ExitCode, runErr)
		}
	}
	if err := validateEvidenceOutputRoot(
		context.Background(), ProcessRunner{}, repository, outputRoot, leafRunID,
	); err == nil || !strings.Contains(err.Error(), "evidence/") {
		t.Fatalf("leaf-only ignore patterns authorized an unignored output directory: %v", err)
	}
	ignore, err = os.ReadFile(ignorePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignorePath, append(ignore, []byte("/evidence/\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateEvidenceOutputRoot(
		context.Background(), ProcessRunner{}, repository, outputRoot, "ignored-authority",
	); err != nil {
		t.Fatalf("ignored repository output was rejected: %v", err)
	}
	if err := validateEvidenceOutputRoot(
		context.Background(), ProcessRunner{}, repository, filepath.Join(t.TempDir(), "outside"), "outside-authority",
	); err != nil {
		t.Fatalf("external output was subjected to repository ignore rules: %v", err)
	}
}

func TestCommittedInputClassificationUsesCapturedCommitOID(t *testing.T) {
	commit := strings.Repeat("c", 40)
	var treeRevision string
	runner := commandFunc(func(_ context.Context, command Command) (CommandResult, error) {
		operation := command.Arguments[2]
		switch operation {
		case "rev-parse":
			return CommandResult{ExitCode: 0, Output: []byte("sha1\n")}, nil
		case "ls-tree":
			treeRevision = command.Arguments[len(command.Arguments)-1]
			return CommandResult{ExitCode: 0}, nil
		default:
			return CommandResult{}, errors.New("unexpected Git operation")
		}
	})
	if _, _, err := classifyCommittedInputs(
		context.Background(), runner, t.TempDir(), commit, nil, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	if treeRevision != commit {
		t.Fatalf("classification dereferenced %q instead of captured commit %q", treeRevision, commit)
	}
}

func TestOutputAuthorityValidationPrecedesRecoveryAndStageCreation(t *testing.T) {
	repository := newSnapshotFixtureRepository(t)
	outputRoot := filepath.Join(repository, "untrusted-evidence")
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.close(); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(outputRoot, ".staging-abandoned", "sentinel.txt")
	if err := os.MkdirAll(filepath.Dir(abandoned), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abandoned, []byte("retain until authority is trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * abandonedStageMinimumAge)
	if err := os.Chtimes(filepath.Dir(abandoned), old, old); err != nil {
		t.Fatal(err)
	}
	application := Application{
		Commands:        ProcessRunner{},
		NewRunID:        func() (string, error) { return "validation-order", nil },
		MutationDomains: passthroughMutationDomainFactory{},
	}
	_, err = application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository, OutputRoot: outputRoot, SampleCount: 1,
		WorkloadIDs: []string{"ready-scaling"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be Git-ignored") {
		t.Fatalf("untrusted output authority advanced to recovery: %v", err)
	}
	if content, readErr := os.ReadFile(abandoned); readErr != nil || string(content) == "" {
		t.Fatalf("pre-validation recovery changed abandoned stage: content=%q err=%v", content, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(outputRoot, ".runtime-validation-order")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("runtime child was created before output validation: %v", statErr)
	}
}

func newSnapshotFixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":            "module example.com/performance-snapshot\n\ngo 1.22\n",
		"go.work":           "go 1.22\n\nuse .\n",
		".gitignore":        "sample/generated.go\n",
		"sample/product.go": "package sample\n\nfunc ProductValue() int { return 1 }\n",
		"sample/sample_perfevidence_test.go": `package sample

import "testing"

func BenchmarkOwned(b *testing.B) {
	for range b.N {
		_ = ProductValue()
	}
	b.ReportMetric(1, "objects/op")
}
`,
		"sample/correctness_test.go": `package sample

import (
	_ "example.com/performance-snapshot/internal/testfixture"
	"testing"
)

func TestCorrectness(t *testing.T) { t.Fatal("must never enter performance binary") }
`,
		"internal/testfixture/fixture.go": `package testfixture

func init() { panic("correctness fixture entered performance binary") }
`,
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{
		{"init"},
		{"add", "."},
		{"-c", "user.name=WindShare Test", "-c", "user.email=windshare@example.invalid", "commit", "-m", "fixture"},
	}
	for _, arguments := range commands {
		result, err := (ProcessRunner{}).Run(context.Background(), Command{
			Executable: "git", Arguments: arguments, Directory: root,
		})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("git %v: %v: %s", arguments, err, result.Output)
		}
	}
	return root
}

func snapshotFixtureWorkload() Workload {
	return Workload{
		ID: "owned", ModuleDir: ".", Package: "./sample",
		Benchmark: "BenchmarkOwned", BenchTime: "1x",
		Contracts: []BenchmarkContract{{
			Name: "BenchmarkOwned", RequiredMetrics: []string{"ns/op", "B/op", "allocs/op", "objects/op"},
		}},
		HardOracles: []MetricOracle{{
			ID: "owned-object", Benchmark: "BenchmarkOwned", Metric: "objects/op", Comparison: Equal, Limit: 1,
		}},
	}
}

func cloneSnapshotFixture(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (ProcessRunner{}).Run(context.Background(), Command{
		Executable: "git", Arguments: []string{"clone", "--quiet", "--no-local", source, destination},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("clone snapshot fixture: %v: %s", err, result.Output)
	}
}

func containsCanonicalPath(value, path string) bool {
	return strings.Contains(
		strings.ToLower(filepath.ToSlash(value)),
		strings.ToLower(filepath.ToSlash(filepath.Clean(path))),
	)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
