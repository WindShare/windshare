package perfevidence

import (
	"context" // Git SHA-1 repositories require the native object algorithm, not a security primitive.
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

const snapshotDirectoryName = "source-snapshot"

type PreparedWorkload struct {
	ModuleRoot  string
	Package     string
	OverlayPath string
	Graph       BuildGraphIdentity
}
type PreparedSnapshot struct {
	Root        string
	Environment controlledGoEnvironment
	Identity    SnapshotIdentity
	Workloads   map[string]PreparedWorkload
	revalidator snapshotRevalidator
	authority   byteConsumptionAuthority
	domain      MutationDomain
}
type inventoryFile struct {
	Logical           string
	Physical          string
	Origin            string
	WorkspaceRelative string
	Mode              os.FileMode
	Bytes             int64
	SHA256            string
}
type workloadInventory struct {
	Files    []inventoryFile
	Modules  []ModuleIdentity
	Packages []string
	Closure  string
}
type inventoryContext struct {
	RepositoryRoot string
	WorkspaceRoot  string
	GoRoot         string
	GoModCache     string
	GoCache        string
	Temporary      string
	Overlay        workloadOverlay
}

func prepareSnapshot(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	artifactRoot string,
	runtimeRoot string,
	workloads []Workload,
	mutationDomains MutationDomainFactory,
) (result PreparedSnapshot, resultErr error) {
	var authority byteConsumptionAuthority
	var domain MutationDomain
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeConsumptionAuthority(authority))
			if domain != nil {
				resultErr = errors.Join(resultErr, domain.Close())
			}
		}
	}()
	environment, err := prepareControlledGoEnvironment(ctx, runner, repositoryRoot, runtimeRoot)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	authority = environment.Authority
	gitIdentity, err := CaptureSource(ctx, runner, repositoryRoot)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("capture Git identity: %w", err)
	}
	overlays, err := discoverAndPrefetchWorkloads(
		ctx, runner, environment, repositoryRoot, runtimeRoot, workloads,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	environment, err = isolateControlledGoEnvironment(environment, runtimeRoot)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	liveBefore, err := inventoryLiveWorkloads(
		ctx, runner, environment, repositoryRoot, workloads, overlays,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	if err := validateWorkloadInventoryBudget(liveBefore); err != nil {
		return PreparedSnapshot{}, err
	}
	snapshotRoot, workspaceRoot, union, prepared, finalOverlays, err := materializeSnapshotWorkloads(
		repositoryRoot, artifactRoot, liveBefore, workloads, overlays,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	// A second live traversal closes the materialization window. A mutation is
	// never tolerated merely because it was later reverted in the worktree.
	if err := verifyLiveClosuresUnchanged(
		ctx, runner, environment, repositoryRoot, workloads, overlays, liveBefore,
	); err != nil {
		return PreparedSnapshot{}, err
	}
	if err := requireStableSourceObservation(
		ctx, runner, repositoryRoot, gitIdentity, "snapshot materialization",
	); err != nil {
		return PreparedSnapshot{}, err
	}
	buildCache := filepath.Join(runtimeRoot, "sealed-build-cache")
	if err := os.Mkdir(buildCache, 0o700); err != nil {
		return PreparedSnapshot{}, fmt.Errorf("create sealed build cache: %w", err)
	}
	environment.GoCache = buildCache
	environment.Offline = replaceEnvironmentValue(environment.Offline, "GOCACHE", buildCache)
	environment.Evidence = replaceEvidenceEnvironment(environment.Evidence, "GOCACHE", buildCache)
	snapshotWork := filepath.Join(workspaceRoot, "go.work")
	snapshotEnvironment := environment.withWorkspace(snapshotWork, false)
	if err := sealSnapshotTree(snapshotRoot); err != nil {
		return PreparedSnapshot{}, err
	}
	if err := refreshInventoryModes(union); err != nil {
		return PreparedSnapshot{}, err
	}
	consumptionFiles, err := inventoryConsumptionUniverse(map[string]string{
		"snapshot":   snapshotRoot,
		"goroot":     environment.ToolchainLocations.GoRoot,
		"gomodcache": environment.GoModCache,
	})
	if err != nil {
		return PreparedSnapshot{}, err
	}
	consumptionInputs, err := canonicalSourceFiles(consumptionFiles)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	authorityRoots := append([]string{snapshotRoot}, environment.AuthorityRoots...)
	finalAuthority, err := acquireConsumptionAuthority(inventoryValidationTargets(consumptionFiles), authorityRoots)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("acquire immutable consumption authority: %w", err)
	}
	authority = combineConsumptionAuthorities(authority, finalAuthority)
	environment.Authority = authority
	authoritativeRunner := runner
	if mutationDomains != nil {
		domain, err = mutationDomains.Open(ctx, MutationDomainSpec{
			RuntimeRoot: runtimeRoot,
			Roots: []MutationRoot{
				{Name: "snapshot", HostPath: snapshotRoot},
				{Name: "goroot", HostPath: environment.ToolchainLocations.GoRoot},
				{Name: "gomodcache", HostPath: environment.GoModCache},
			},
		})
		if err != nil {
			return PreparedSnapshot{}, fmt.Errorf("open authoritative snapshot mutation domain: %w", err)
		}
		authoritativeRunner, err = runnerWithMutationDomain(runner, domain)
		if err != nil {
			return PreparedSnapshot{}, err
		}
		if err := verifyDownloadedModulesUnderAuthority(
			ctx, authoritativeRunner, environment, workspaceRoot, workloads, authority,
		); err != nil {
			return PreparedSnapshot{}, fmt.Errorf("verify modules in authoritative mutation domain: %w", err)
		}
	}
	prepared, finalFiles, modules, graphs, err := inventorySealedWorkloads(
		ctx, authoritativeRunner, environment, snapshotEnvironment, artifactRoot, workspaceRoot,
		workloads, finalOverlays, liveBefore, prepared,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	finalFiles = append(finalFiles, union...)
	files, err := canonicalSourceFiles(finalFiles)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	committed, uncommitted, err := classifyCommittedInputs(
		ctx, runner, repositoryRoot, gitIdentity.Commit, files, workspaceRoot,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	effective, err := inspectEffectiveGoEnvironment(
		ctx, authoritativeRunner, environment.GoExecutable, workspaceRoot, snapshotEnvironment,
	)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	controlledEnvironment := replaceEvidenceEnvironment(environment.Evidence, "GOWORK", snapshotWork)
	controlledEnvironment = replaceEvidenceEnvironment(controlledEnvironment, "GOCACHE", buildCache)
	buildEnvironment, err := comparableBuildEnvironment(controlledEnvironment, map[string]semanticPath{
		"GOCACHE":    {Physical: buildCache, Logical: "$RUNTIME/sealed-build-cache"},
		"GOMODCACHE": {Physical: environment.GoModCache, Logical: "$RUNTIME/gomodcache"},
		"GOWORK":     {Physical: snapshotWork, Logical: "$SNAPSHOT/workspace/go.work"},
		"TEMP":       {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
		"TMP":        {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
		"TMPDIR":     {Physical: environment.Temporary, Logical: "$RUNTIME/tmp"},
	})
	if err != nil {
		return PreparedSnapshot{}, err
	}
	processEnvironment, err := processEnvironmentEvidence(snapshotEnvironment)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	overlayFiles, err := overlayFileDiagnostics(artifactRoot, finalOverlays)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	identity := SnapshotIdentity{
		Git: gitIdentity, Files: committed, ConsumptionInputs: consumptionInputs,
		CompiledInputsMatchCommit: len(uncommitted) == 0,
		UncommittedInputs:         uncommitted,
		BuildEnvironment:          buildEnvironment,
		Toolchain:                 environment.Toolchain,
		Modules:                   modules,
		BuildGraphs:               graphs,
		Diagnostics: SnapshotDiagnostics{
			ProcessEnvironment: processEnvironment,
			EffectiveGoEnv:     effective,
			Toolchain:          environment.ToolchainLocations,
			OverlayFiles:       overlayFiles,
		},
	}
	identity.SHA256, err = snapshotIdentitySHA(identity)
	if err != nil {
		return PreparedSnapshot{}, err
	}
	validationInputs := append(append([]inventoryFile(nil), finalFiles...), consumptionFiles...)
	revalidator, err := newSnapshotValidationPlan(artifactRoot, validationInputs, identity, environment)
	if err != nil {
		return PreparedSnapshot{}, fmt.Errorf("prepare final-byte validation: %w", err)
	}
	if err := requireStableSourceObservation(
		ctx, runner, repositoryRoot, gitIdentity, "snapshot identity finalization",
	); err != nil {
		return PreparedSnapshot{}, err
	}
	environment.Offline = snapshotEnvironment
	environment.Effective = effective
	result = PreparedSnapshot{
		Root: snapshotRoot, Environment: environment, Identity: identity, Workloads: prepared,
		revalidator: revalidator, authority: authority, domain: domain,
	}
	return result, nil
}

func inventoryValidationTargets(files []inventoryFile) []snapshotValidationTarget {
	targets := make([]snapshotValidationTarget, 0, len(files))
	for _, file := range files {
		targets = append(targets, snapshotValidationTarget{
			LogicalPath: file.Logical, PhysicalPath: file.Physical, Bytes: file.Bytes, SHA256: file.SHA256,
		})
	}
	return targets
}

func validateWorkloadInventoryBudget(inventories map[string]workloadInventory) error {
	meter, err := newSnapshotInputMeter(defaultSnapshotInputBudget())
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	workloadIDs := make([]string, 0, len(inventories))
	for workloadID := range inventories {
		workloadIDs = append(workloadIDs, workloadID)
	}
	sort.Strings(workloadIDs)
	for _, workloadID := range workloadIDs {
		for _, file := range inventories[workloadID].Files {
			key := canonicalPath(file.Physical)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			if err := meter.observe(file.Logical, true, file.Bytes); err != nil {
				return fmt.Errorf("workload %s input budget: %w", workloadID, err)
			}
		}
	}
	return nil
}

func refreshInventoryModes(files []inventoryFile) error {
	for index := range files {
		information, err := os.Lstat(files[index].Physical)
		if err != nil || !information.Mode().IsRegular() || isReparsePointInfo(information) {
			return errors.Join(fmt.Errorf("refresh sealed input mode for %s", files[index].Logical), err)
		}
		files[index].Mode = information.Mode()
	}
	return nil
}

func runnerWithMutationDomain(runner CommandRunner, domain MutationDomain) (CommandRunner, error) {
	switch concrete := runner.(type) {
	case ProcessRunner:
		concrete.MutationDomain = domain
		return concrete, nil
	case *ProcessRunner:
		copy := *concrete
		copy.MutationDomain = domain
		return copy, nil
	case interface {
		withMutationDomain(MutationDomain) CommandRunner
	}:
		return concrete.withMutationDomain(domain), nil
	default:
		return nil, errors.New("authoritative snapshot mutation domain requires the production process runner")
	}
}

const (
	benchmarkBuildTimeout    = 5 * time.Minute
	benchmarkSampleTimeout   = 15 * time.Minute
	profileValidationTimeout = 2 * time.Minute
)

type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	RunID       string    `json:"run_id"`
	OperationID string    `json:"operation_id"`
	Scenario    string    `json:"scenario"`
	Component   string    `json:"component"`
	Milestone   string    `json:"milestone"`
	Outcome     string    `json:"outcome"`
	Detail      string    `json:"detail,omitempty"`
}
type EventLogger interface {
	Log(Event) error
}
type Runner struct {
	Commands CommandRunner
	Logger   EventLogger
	RunID    string
}

func (runner Runner) Measure(
	ctx context.Context,
	stageRoot string,
	workload Workload,
	prepared PreparedWorkload,
	environment controlledGoEnvironment,
	sampleCount int,
	profile bool,
) (evidence WorkloadEvidence, resultErr error) {
	if err := validateSampleCount(sampleCount); err != nil {
		return WorkloadEvidence{}, err
	}
	moduleRoot := prepared.ModuleRoot
	if moduleRoot == "" || prepared.OverlayPath == "" || environment.GoExecutable == "" {
		return WorkloadEvidence{}, errors.New("workload requires a sealed build plan")
	}
	evidence = WorkloadEvidence{Definition: workload}
	buildOperationID := workload.ID
	if err := runner.beginPhase(&evidence.Build, buildOperationID, EvidencePhaseBuild); err != nil {
		return evidence, err
	}
	buildFinished := false
	defer func() {
		if !buildFinished {
			buildFinished = true
			resultErr = errors.Join(
				resultErr,
				runner.finishPhase(&evidence.Build, buildOperationID, resultErr),
			)
		}
	}()
	binarySuffix := ".test"
	if runtime.GOOS == "windows" {
		binarySuffix += ".exe"
	}
	binaryRelative := filepath.ToSlash(filepath.Join("binaries", workload.ID+binarySuffix))
	binaryPath := filepath.Join(stageRoot, filepath.FromSlash(binaryRelative))
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		return evidence, fmt.Errorf("create binary directory: %w", err)
	}
	buildCommand := Command{
		Executable: environment.GoExecutable,
		Arguments: []string{
			"test", "-a", "-mod=readonly", "-c", "-trimpath", "-buildvcs=false",
			"-overlay", prepared.OverlayPath, "-o", binaryPath, prepared.Package,
		},
		Directory: moduleRoot, Environment: environment.Offline, ReplaceEnvironment: true,
		authorities:      []byteConsumptionAuthority{environment.Authority},
		mutationIntent:   mutationIntentArtifactProduction,
		protectedOutputs: []MutationOutput{{HostPath: binaryPath, MaxBytes: maximumBinaryBytes}},
	}
	buildResult, buildErr := runner.runWithTimeout(ctx, benchmarkBuildTimeout, buildCommand)
	buildLog := filepath.ToSlash(filepath.Join("logs", workload.ID, "build.log"))
	evidence.Build = commandEvidence(
		buildCommand, buildResult, workload.ModuleDir, EvidencePhaseBuild,
	)
	buildLogArtifact, err := writeCommandLog(stageRoot, buildLog, buildResult.Output)
	if err != nil {
		return evidence, fmt.Errorf("write build log: %w", err)
	}
	evidence.Build.Artifacts = append(evidence.Build.Artifacts, buildLogArtifact)
	if buildErr != nil || buildResult.ExitCode != 0 {
		return evidence, commandFailure("build benchmark binary", buildResult, buildErr)
	}
	binaryArtifact, err := inspectArtifactIdentity(stageRoot, binaryRelative)
	if err != nil {
		return evidence, fmt.Errorf("inspect built benchmark binary: %w", err)
	}
	evidence.Build.Artifacts = append(evidence.Build.Artifacts, binaryArtifact)
	binaryTarget, err := artifactValidationTarget(stageRoot, binaryArtifact)
	if err != nil {
		return evidence, err
	}
	binaryAuthority := buildResult.outputAuthorities[binaryPath]
	if binaryAuthority == nil {
		binaryAuthority, err = acquireConsumptionAuthority(
			[]snapshotValidationTarget{binaryTarget}, []string{filepath.Dir(binaryPath)},
		)
		if err != nil {
			return evidence, fmt.Errorf("retain benchmark binary authority: %w", err)
		}
	}
	defer func() { resultErr = errors.Join(resultErr, closeConsumptionAuthority(binaryAuthority)) }()
	binary, err := inspectBinary(
		ctx, runner.Commands, environment, moduleRoot, stageRoot, binaryRelative,
		prepared.Graph.ClosureSHA256, binaryAuthority,
	)
	if err != nil {
		return evidence, err
	}
	evidence.Binary = binary
	buildFinished = true
	if err := runner.finishPhase(&evidence.Build, buildOperationID, nil); err != nil {
		return evidence, err
	}
	for sampleIndex := 1; sampleIndex <= sampleCount; sampleIndex++ {
		sample, sampleErr := runner.measureSample(
			ctx, stageRoot, binaryPath, moduleRoot, workload, environment,
			binaryAuthority, sampleIndex,
		)
		evidence.Samples = append(evidence.Samples, sample)
		if sampleErr != nil {
			return evidence, sampleErr
		}
	}
	evidence.Aggregates, err = AggregateSamples(evidence.Samples, sampleCount)
	if err != nil {
		return evidence, err
	}
	evidence.Oracles = EvaluateOracles(workload, evidence.Samples)
	if !OraclesPassed(evidence.Oracles) {
		return evidence, fmt.Errorf("workload %s failed a hard oracle", workload.ID)
	}
	if profile {
		profileEvidence, profileErr := runner.captureProfile(
			ctx, moduleRoot, stageRoot, binaryPath, workload, environment, binary, binaryAuthority,
		)
		evidence.Profile = &profileEvidence
		if profileErr != nil {
			return evidence, profileErr
		}
	}
	currentBinary, err := inspectBinary(
		ctx, runner.Commands, environment, moduleRoot, stageRoot, binaryRelative,
		prepared.Graph.ClosureSHA256, binaryAuthority,
	)
	if err != nil {
		return evidence, err
	}
	if currentBinary != binary {
		return evidence, fmt.Errorf("benchmark binary %s changed after measurement", workload.ID)
	}
	return evidence, nil
}

func (runner Runner) measureSample(
	ctx context.Context,
	stageRoot string,
	binaryPath string,
	moduleRoot string,
	workload Workload,
	environment controlledGoEnvironment,
	binaryAuthority byteConsumptionAuthority,
	sampleIndex int,
) (sample BenchmarkSample, resultErr error) {
	operationID := fmt.Sprintf("%s-sample-%02d", workload.ID, sampleIndex)
	sample = BenchmarkSample{WorkloadID: workload.ID, Index: sampleIndex}
	if err := runner.beginPhase(&sample.Command, operationID, EvidencePhaseSample); err != nil {
		return sample, err
	}
	finished := false
	defer func() {
		if !finished {
			finished = true
			resultErr = errors.Join(resultErr, runner.finishPhase(&sample.Command, operationID, resultErr))
		}
	}()
	command := benchmarkCommand(
		binaryPath, moduleRoot, workload, environment.Offline, nil,
		environment.Authority, binaryAuthority,
	)
	result, runErr := runner.runWithTimeout(ctx, benchmarkSampleTimeout, command)
	sample.Command = commandEvidence(command, result, workload.ModuleDir, EvidencePhaseSample)
	logRelative := filepath.ToSlash(filepath.Join("logs", workload.ID, fmt.Sprintf("sample-%02d.log", sampleIndex)))
	logArtifact, err := writeCommandLog(stageRoot, logRelative, result.Output)
	if err != nil {
		return sample, fmt.Errorf("write benchmark sample log: %w", err)
	}
	sample.Command.Artifacts = append(sample.Command.Artifacts, logArtifact)
	if runErr != nil || result.ExitCode != 0 {
		return sample, commandFailure("run benchmark sample", result, runErr)
	}
	rows, err := ParseBenchmarkOutput(result.Output)
	if err != nil {
		return sample, fmt.Errorf("parse workload %s sample %d: %w", workload.ID, sampleIndex, err)
	}
	if err := ValidateSample(workload, rows); err != nil {
		return sample, fmt.Errorf("validate workload %s sample %d: %w", workload.ID, sampleIndex, err)
	}
	sample.Rows = rows
	finished = true
	if err := runner.finishPhase(&sample.Command, operationID, nil); err != nil {
		return sample, err
	}
	return sample, nil
}

func (runner Runner) runWithTimeout(ctx context.Context, timeout time.Duration, command Command) (CommandResult, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runner.Commands.Run(commandContext, command)
}

func (runner Runner) beginPhase(
	evidence *CommandEvidence,
	operationID string,
	phase EvidencePhase,
) error {
	evidence.Phase = phase
	if err := runner.log(operationID, string(phase)+"-started", "running", ""); err != nil {
		setCommandOutcome(evidence, fmt.Errorf("log %s start: %w", phase, err))
		return fmt.Errorf("log %s start: %w", phase, err)
	}
	return nil
}

func (runner Runner) finishPhase(
	evidence *CommandEvidence,
	operationID string,
	phaseErr error,
) error {
	setCommandOutcome(evidence, phaseErr)
	outcome := string(evidence.Outcome)
	detail := evidence.Error
	if err := runner.log(operationID, string(evidence.Phase)+"-finished", outcome, detail); err != nil {
		logErr := fmt.Errorf("log %s completion: %w", evidence.Phase, err)
		setCommandOutcome(evidence, errors.Join(phaseErr, logErr))
		return logErr
	}
	return nil
}

func setCommandOutcome(evidence *CommandEvidence, phaseErr error) {
	if phaseErr == nil {
		evidence.Outcome = EvidenceOutcomeSucceeded
		evidence.Error = ""
		return
	}
	evidence.Outcome = EvidenceOutcomeFailed
	evidence.Error = phaseErr.Error()
}

func (runner Runner) log(operationID, milestone, outcome, detail string) error {
	if runner.Logger == nil {
		return nil
	}
	return runner.Logger.Log(Event{
		Timestamp: time.Now().UTC(),
		RunID:     runner.RunID, OperationID: operationID, Scenario: workloadScenario(operationID), Component: "perfevidence",
		Milestone: milestone, Outcome: outcome, Detail: detail,
	})
}

func commandEvidence(
	command Command,
	result CommandResult,
	directory string,
	phase EvidencePhase,
) CommandEvidence {
	return CommandEvidence{
		Phase:      phase,
		Executable: filepath.Base(command.Executable), Arguments: append([]string(nil), command.Arguments...),
		Directory: filepath.ToSlash(directory), ProcessID: result.ProcessID, ExitCode: result.ExitCode,
		StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
	}
}
