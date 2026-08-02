package perfevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

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

func (runner Runner) captureProfile(
	ctx context.Context,
	moduleRoot string,
	stageRoot string,
	binaryPath string,
	workload Workload,
	environment controlledGoEnvironment,
	binary BinaryEvidence,
	binaryAuthority byteConsumptionAuthority,
) (evidence ProfileEvidence, resultErr error) {
	operationID := workload.ID + "-profile"
	if err := runner.beginPhase(&evidence.Command, operationID, EvidencePhaseProfile); err != nil {
		return evidence, err
	}
	finished := false
	defer func() {
		if !finished {
			finished = true
			resultErr = errors.Join(resultErr, runner.finishPhase(&evidence.Command, operationID, resultErr))
		}
	}()
	cpuRelative := filepath.ToSlash(filepath.Join("profiles", workload.ID, "cpu.pprof"))
	memoryRelative := filepath.ToSlash(filepath.Join("profiles", workload.ID, "memory.pprof"))
	cpuPath := filepath.Join(stageRoot, filepath.FromSlash(cpuRelative))
	memoryPath := filepath.Join(stageRoot, filepath.FromSlash(memoryRelative))
	if err := os.MkdirAll(filepath.Dir(cpuPath), 0o700); err != nil {
		return evidence, fmt.Errorf("create profile directory: %w", err)
	}
	command := benchmarkCommand(binaryPath, moduleRoot, workload, environment.Offline, []string{
		"-test.cpuprofile=" + cpuPath,
		"-test.memprofile=" + memoryPath,
	}, environment.Authority, binaryAuthority)
	command.protectedOutputs = []MutationOutput{
		{HostPath: cpuPath, MaxBytes: maximumProfileBytes},
		{HostPath: memoryPath, MaxBytes: maximumProfileBytes},
	}
	command.mutationIntent = mutationIntentArtifactProduction
	result, runErr := runner.runWithTimeout(ctx, benchmarkSampleTimeout, command)
	logRelative := filepath.ToSlash(filepath.Join("logs", workload.ID, "profile.log"))
	evidence.Command = commandEvidence(command, result, workload.ModuleDir, EvidencePhaseProfile)
	logArtifact, err := writeCommandLog(stageRoot, logRelative, result.Output)
	if err != nil {
		return evidence, fmt.Errorf("write profile log: %w", err)
	}
	evidence.Command.Artifacts = append(evidence.Command.Artifacts, logArtifact)
	evidence.Binary = artifactFromBinary(binary)
	if runErr != nil || result.ExitCode != 0 {
		return evidence, commandFailure("capture benchmark profile", result, runErr)
	}
	cpuIdentity, err := inspectArtifactIdentity(stageRoot, cpuRelative)
	if err != nil {
		return evidence, fmt.Errorf("inspect CPU profile: %w", err)
	}
	evidence.CPU = cpuIdentity
	evidence.Command.Artifacts = append(evidence.Command.Artifacts, cpuIdentity)
	memoryIdentity, err := inspectArtifactIdentity(stageRoot, memoryRelative)
	if err != nil {
		return evidence, fmt.Errorf("inspect memory profile: %w", err)
	}
	evidence.Memory = memoryIdentity
	evidence.Command.Artifacts = append(evidence.Command.Artifacts, memoryIdentity)
	if evidence.CPU.Bytes == 0 || evidence.Memory.Bytes == 0 {
		return evidence, errors.New("captured profiles must be non-empty")
	}
	cpuTarget, err := artifactValidationTarget(stageRoot, evidence.CPU)
	if err != nil {
		return evidence, err
	}
	memoryTarget, err := artifactValidationTarget(stageRoot, evidence.Memory)
	if err != nil {
		return evidence, err
	}
	profileAuthority := combineConsumptionAuthorities(
		result.outputAuthorities[cpuPath], result.outputAuthorities[memoryPath],
	)
	if profileAuthority == nil {
		profileAuthority, err = acquireConsumptionAuthority(
			[]snapshotValidationTarget{cpuTarget, memoryTarget}, []string{filepath.Dir(cpuPath)},
		)
		if err != nil {
			return evidence, fmt.Errorf("retain profile byte authority: %w", err)
		}
	}
	defer func() { resultErr = errors.Join(resultErr, closeConsumptionAuthority(profileAuthority)) }()
	profiles := []struct {
		name     string
		path     string
		identity ArtifactFile
	}{
		{name: "cpu", path: cpuPath, identity: evidence.CPU},
		{name: "memory", path: memoryPath, identity: evidence.Memory},
	}
	for _, profile := range profiles {
		if err := requireProfileInputs(stageRoot, evidence.Binary, profile.identity); err != nil {
			return evidence, fmt.Errorf("validate %s pprof inputs: %w", profile.name, err)
		}
		verificationEvidence, verificationErr := runner.verifyProfile(
			ctx, moduleRoot, stageRoot, binaryPath, workload, environment,
			evidence.Binary, profile.name, profile.path, profile.identity,
			environment.Authority, binaryAuthority, profileAuthority,
		)
		evidence.Verification = append(evidence.Verification, verificationEvidence)
		if verificationErr != nil {
			return evidence, verificationErr
		}
	}
	if err := requireProfileInputs(stageRoot, evidence.Binary, evidence.CPU, evidence.Memory); err != nil {
		return evidence, fmt.Errorf("profile inputs changed after verification: %w", err)
	}
	finished = true
	if err := runner.finishPhase(&evidence.Command, operationID, nil); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func (runner Runner) verifyProfile(
	ctx context.Context,
	moduleRoot string,
	stageRoot string,
	binaryPath string,
	workload Workload,
	environment controlledGoEnvironment,
	binary ArtifactFile,
	profileName string,
	profilePath string,
	profile ArtifactFile,
	authorities ...byteConsumptionAuthority,
) (evidence CommandEvidence, resultErr error) {
	operationID := workload.ID + "-profile-" + profileName + "-verification"
	if err := runner.beginPhase(&evidence, operationID, EvidencePhaseProfileVerification); err != nil {
		return evidence, err
	}
	finished := false
	defer func() {
		if !finished {
			finished = true
			resultErr = errors.Join(resultErr, runner.finishPhase(&evidence, operationID, resultErr))
		}
	}()
	command := Command{
		Executable:  environment.GoExecutable,
		Arguments:   []string{"tool", "pprof", "-raw", binaryPath, profilePath},
		Directory:   moduleRoot,
		Environment: environment.Offline, ReplaceEnvironment: true,
		authorities:    append([]byteConsumptionAuthority(nil), authorities...),
		mutationIntent: mutationIntentVerification,
	}
	result, runErr := runner.runWithTimeout(ctx, profileValidationTimeout, command)
	evidence = commandEvidence(command, result, workload.ModuleDir, EvidencePhaseProfileVerification)
	logRelative := filepath.ToSlash(filepath.Join(
		"logs", workload.ID, "profile-"+profileName+"-verification.log",
	))
	verificationLog, err := writeCommandLog(stageRoot, logRelative, result.Output)
	if err != nil {
		return evidence, fmt.Errorf("write %s profile verification log: %w", profileName, err)
	}
	evidence.Artifacts = append(evidence.Artifacts, verificationLog)
	if runErr != nil || result.ExitCode != 0 {
		return evidence, commandFailure("validate "+profileName+" pprof", result, runErr)
	}
	if err := requireProfileInputs(stageRoot, binary, profile); err != nil {
		return evidence, fmt.Errorf("%s pprof inputs changed while parsing: %w", profileName, err)
	}
	finished = true
	if err := runner.finishPhase(&evidence, operationID, nil); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func benchmarkCommand(
	binaryPath, moduleRoot string,
	workload Workload,
	environment, extra []string,
	authorities ...byteConsumptionAuthority,
) Command {
	arguments := make([]string, 0, 5+len(extra))
	arguments = append(arguments,
		"-test.run=^$",
		"-test.bench=^"+workload.Benchmark+"$",
		"-test.benchmem",
		"-test.benchtime="+workload.BenchTime,
		"-test.count=1",
	)
	arguments = append(arguments, extra...)
	return Command{
		Executable: binaryPath, Arguments: arguments, Directory: moduleRoot,
		Environment: environment, ReplaceEnvironment: true,
		authorities:    append([]byteConsumptionAuthority(nil), authorities...),
		mutationIntent: mutationIntentVerification,
	}
}

func inspectBinary(
	ctx context.Context,
	commands CommandRunner,
	environment controlledGoEnvironment,
	moduleRoot string,
	stageRoot string,
	relative string,
	buildGraphSHA256 string,
	binaryAuthority byteConsumptionAuthority,
) (BinaryEvidence, error) {
	artifact, err := inspectArtifactIdentity(stageRoot, relative)
	if err != nil {
		return BinaryEvidence{}, fmt.Errorf("inspect benchmark binary: %w", err)
	}
	path := filepath.Join(stageRoot, filepath.FromSlash(relative))
	result, runErr := commands.Run(ctx, Command{
		Executable: environment.GoExecutable, Arguments: []string{"tool", "buildid", path}, Directory: moduleRoot,
		Environment: environment.Offline, ReplaceEnvironment: true,
		authorities:    []byteConsumptionAuthority{environment.Authority, binaryAuthority},
		mutationIntent: mutationIntentVerification,
	})
	if runErr != nil || result.ExitCode != 0 {
		return BinaryEvidence{}, commandFailure("read Go build ID", result, runErr)
	}
	buildID := strings.TrimSpace(string(commandStdout(result)))
	if buildID == "" {
		return BinaryEvidence{}, errors.New("benchmark binary has an empty Go build ID")
	}
	metadata, metadataErr := commands.Run(ctx, Command{
		Executable: environment.GoExecutable, Arguments: []string{"version", "-m", path}, Directory: moduleRoot,
		Environment: environment.Offline, ReplaceEnvironment: true,
		authorities:    []byteConsumptionAuthority{environment.Authority, binaryAuthority},
		mutationIntent: mutationIntentVerification,
	})
	if metadataErr != nil || metadata.ExitCode != 0 {
		return BinaryEvidence{}, commandFailure("read Go binary metadata", metadata, metadataErr)
	}
	versionMetadata, err := canonicalGoVersionMetadata(commandStdout(metadata), path, relative)
	if err != nil {
		return BinaryEvidence{}, err
	}
	if err := rejectTransientMetadataPaths(
		versionMetadata, stageRoot, moduleRoot, environment.GoCache, environment.GoModCache, environment.Temporary,
	); err != nil {
		return BinaryEvidence{}, err
	}
	current, err := inspectArtifactIdentity(stageRoot, relative)
	if err != nil {
		return BinaryEvidence{}, fmt.Errorf("reinspect benchmark binary: %w", err)
	}
	if current != artifact {
		return BinaryEvidence{}, errors.New("benchmark binary changed while its metadata was inspected")
	}
	return BinaryEvidence{
		Path: artifact.Path, Bytes: artifact.Bytes, SHA256: artifact.SHA256, GoBuildID: buildID,
		GoVersionMetadata: versionMetadata, BuildGraphSHA256: buildGraphSHA256,
	}, nil
}

func artifactFromBinary(binary BinaryEvidence) ArtifactFile {
	return ArtifactFile{Path: binary.Path, Bytes: binary.Bytes, SHA256: binary.SHA256}
}

func requireProfileInputs(stageRoot string, expected ...ArtifactFile) error {
	for _, identity := range expected {
		observed, err := inspectArtifactIdentity(stageRoot, identity.Path)
		if err != nil {
			return err
		}
		if observed != identity {
			return fmt.Errorf("artifact %s no longer has the parsed byte identity", identity.Path)
		}
	}
	return nil
}

func inspectArtifactIdentity(stageRoot, relative string) (ArtifactFile, error) {
	authority, err := openTreeAuthority(stageRoot)
	if err != nil {
		return ArtifactFile{}, err
	}
	wanted := filepath.ToSlash(relative)
	var identity ArtifactFile
	walkErr := authority.walkRegularFiles(func(path string, file *os.File, info os.FileInfo) error {
		if path != wanted {
			return nil
		}
		observed, hashErr := artifactIdentityFromOpenFile(file, info, path)
		if hashErr == nil {
			identity = observed
		}
		return hashErr
	})
	if err := errors.Join(walkErr, authority.close()); err != nil {
		return ArtifactFile{}, err
	}
	if identity.Path == "" {
		return ArtifactFile{}, fmt.Errorf("artifact %s was absent from the retained stage", wanted)
	}
	return identity, nil
}

func canonicalGoVersionMetadata(encoded []byte, binaryPath, logicalPath string) (string, error) {
	metadata := strings.TrimSpace(string(encoded))
	if metadata == "" {
		return "", errors.New("benchmark binary has empty Go version metadata")
	}
	lines := strings.Split(metadata, "\n")
	physicalPrefixes := []string{binaryPath + ": ", filepath.ToSlash(binaryPath) + ": "}
	version := ""
	for _, prefix := range physicalPrefixes {
		if len(lines[0]) >= len(prefix) && strings.EqualFold(lines[0][:len(prefix)], prefix) {
			version = lines[0][len(prefix):]
			break
		}
	}
	if strings.TrimSpace(version) == "" {
		return "", errors.New("go version metadata did not identify the inspected benchmark binary")
	}
	lines[0] = filepath.ToSlash(logicalPath) + ": " + version
	return strings.Join(lines, "\n"), nil
}

func rejectTransientMetadataPaths(metadata string, paths ...string) error {
	canonicalMetadata := strings.ToLower(filepath.ToSlash(metadata))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve transient metadata path: %w", err)
		}
		canonicalPath := strings.ToLower(filepath.ToSlash(filepath.Clean(absolute)))
		if strings.Contains(canonicalMetadata, canonicalPath) {
			return fmt.Errorf("go version metadata leaked transient build path %s", path)
		}
	}
	return nil
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

func writeCommandLog(stageRoot, relative string, content []byte) (ArtifactFile, error) {
	if err := writeExclusive(filepath.Join(stageRoot, filepath.FromSlash(relative)), content); err != nil {
		return ArtifactFile{}, err
	}
	return inspectArtifactIdentity(stageRoot, relative)
}

func commandFailure(action string, result CommandResult, err error) error {
	message := strings.TrimSpace(string(result.Output))
	if len(message) > 1_024 {
		message = message[len(message)-1_024:]
	}
	if err != nil {
		return fmt.Errorf("%s (exit %d): %w: %s", action, result.ExitCode, err, message)
	}
	return fmt.Errorf("%s exited with %d: %s", action, result.ExitCode, message)
}

func workloadScenario(operationID string) string {
	if workload, _, found := strings.Cut(operationID, "-sample-"); found {
		return workload
	}
	if workload, _, found := strings.Cut(operationID, "-profile"); found {
		return workload
	}
	return operationID
}

func writeExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, err := file.Write(content)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if written != len(content) {
		return errors.Join(
			fmt.Errorf("short evidence write: wrote %d of %d bytes", written, len(content)), file.Close(),
		)
	}
	return file.Close()
}
