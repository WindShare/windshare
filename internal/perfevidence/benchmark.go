package perfevidence

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

var benchmarkNamePattern = regexp.MustCompile(`^(Benchmark\S+?)-\d+$`)

func ParseBenchmarkOutput(output []byte) ([]BenchmarkReading, error) {
	var readings []BenchmarkReading
	scanner := bufio.NewScanner(bytes.NewReader(output))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		nameMatch := benchmarkNamePattern.FindStringSubmatch(fields[0])
		if nameMatch == nil {
			continue
		}
		iterations, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || iterations == 0 {
			return nil, fmt.Errorf("benchmark %s has invalid iteration count %q", nameMatch[1], fields[1])
		}
		if len(fields[2:])%2 != 0 {
			return nil, fmt.Errorf("benchmark %s has an incomplete metric pair", nameMatch[1])
		}
		metrics := make(map[string]float64, len(fields[2:])/2)
		for index := 2; index < len(fields); index += 2 {
			value, parseErr := strconv.ParseFloat(fields[index], 64)
			if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("benchmark %s metric %s has invalid value %q", nameMatch[1], fields[index+1], fields[index])
			}
			unit := fields[index+1]
			if _, duplicate := metrics[unit]; duplicate {
				return nil, fmt.Errorf("benchmark %s repeats metric %s", nameMatch[1], unit)
			}
			metrics[unit] = value
		}
		readings = append(readings, BenchmarkReading{Name: nameMatch[1], Iterations: iterations, Metrics: metrics})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan benchmark output: %w", err)
	}
	if len(readings) == 0 {
		return nil, errors.New("benchmark process produced no parseable benchmark rows")
	}
	return readings, nil
}

func ValidateSample(workload Workload, rows []BenchmarkReading) error {
	if len(rows) != len(workload.Contracts) {
		return fmt.Errorf("workload %s produced %d benchmark rows; want %d", workload.ID, len(rows), len(workload.Contracts))
	}
	byName := make(map[string]BenchmarkReading, len(rows))
	for _, row := range rows {
		if _, duplicate := byName[row.Name]; duplicate {
			return fmt.Errorf("workload %s repeated benchmark %s", workload.ID, row.Name)
		}
		byName[row.Name] = row
	}
	for _, contract := range workload.Contracts {
		row, found := byName[contract.Name]
		if !found {
			return fmt.Errorf("workload %s omitted benchmark %s", workload.ID, contract.Name)
		}
		if len(row.Metrics) != len(contract.RequiredMetrics) {
			return fmt.Errorf(
				"benchmark %s produced %d metrics; contract requires exactly %d",
				contract.Name, len(row.Metrics), len(contract.RequiredMetrics),
			)
		}
		for _, metric := range contract.RequiredMetrics {
			if _, found := row.Metrics[metric]; !found {
				return fmt.Errorf("benchmark %s omitted metric %s", contract.Name, metric)
			}
		}
	}
	return nil
}

func AggregateSamples(samples []BenchmarkSample, expectedSamples int) ([]BenchmarkAggregate, error) {
	if expectedSamples < 1 {
		return nil, errors.New("expected sample count must be positive")
	}
	values := make(map[string]map[string][]float64)
	for _, sample := range samples {
		for _, row := range sample.Rows {
			metrics := values[row.Name]
			if metrics == nil {
				metrics = make(map[string][]float64)
				values[row.Name] = metrics
			}
			for metric, value := range row.Metrics {
				metrics[metric] = append(metrics[metric], value)
			}
		}
	}
	var aggregates []BenchmarkAggregate
	for benchmark, metrics := range values {
		for metric, metricValues := range metrics {
			if len(metricValues) != expectedSamples {
				return nil, fmt.Errorf(
					"benchmark %s metric %s has %d samples; want %d",
					benchmark, metric, len(metricValues), expectedSamples,
				)
			}
			sort.Float64s(metricValues)
			aggregates = append(aggregates, BenchmarkAggregate{
				Benchmark: benchmark,
				Metric:    metric,
				Samples:   len(metricValues),
				Minimum:   metricValues[0],
				P50:       nearestRank(metricValues, 0.50),
				P95:       nearestRank(metricValues, 0.95),
				Maximum:   metricValues[len(metricValues)-1],
			})
		}
	}
	sort.Slice(aggregates, func(left, right int) bool {
		if aggregates[left].Benchmark != aggregates[right].Benchmark {
			return aggregates[left].Benchmark < aggregates[right].Benchmark
		}
		return aggregates[left].Metric < aggregates[right].Metric
	})
	return aggregates, nil
}

func EvaluateOracles(workload Workload, samples []BenchmarkSample) []OracleResult {
	results := make([]OracleResult, 0, len(workload.HardOracles)+1)
	results = append(results, OracleResult{ID: "fresh-process-samples", Passed: freshProcesses(samples)})
	if !results[0].Passed {
		results[0].Error = "one or more samples did not record a distinct process launch"
	}
	for _, oracle := range workload.HardOracles {
		result := OracleResult{
			ID: oracle.ID, Benchmark: oracle.Benchmark, Metric: oracle.Metric,
			Comparison: oracle.Comparison, Limit: oracle.Limit, Passed: true,
		}
		var observations int
		for _, sample := range samples {
			for _, row := range sample.Rows {
				if oracle.Benchmark != "" && row.Name != oracle.Benchmark {
					continue
				}
				value, found := row.Metrics[oracle.Metric]
				if !found {
					continue
				}
				observations++
				if !compare(value, oracle.Comparison, oracle.Limit) {
					result.Passed = false
					result.Error = fmt.Sprintf("%s in %s sample %d was %g", oracle.Metric, row.Name, sample.Index, value)
					break
				}
			}
			if !result.Passed {
				break
			}
		}
		if observations == 0 {
			result.Passed = false
			result.Error = "oracle metric was absent from every matching benchmark row"
		}
		results = append(results, result)
	}
	return results
}

func OraclesPassed(results []OracleResult) bool {
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return true
}

func nearestRank(sorted []float64, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	index = max(index, 0)
	return sorted[index]
}

func freshProcesses(samples []BenchmarkSample) bool {
	if len(samples) == 0 {
		return false
	}
	for _, sample := range samples {
		if sample.Command.ProcessID <= 0 || !sample.Command.FinishedAt.After(sample.Command.StartedAt) {
			return false
		}
	}
	return true
}

func compare(value float64, comparison Comparison, limit float64) bool {
	switch comparison {
	case Equal:
		return value == limit
	case LessThan:
		return value < limit
	case LessThanOrEqual:
		return value <= limit
	case GreaterThan:
		return value > limit
	case GreaterThanOrEqual:
		return value >= limit
	default:
		return false
	}
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

func verifyOwnedProcessStart(
	authorities []byteConsumptionAuthority,
	evidence protocol.StartEvidence,
	executable string,
) error {
	if len(authorities) == 0 {
		return nil
	}
	matched := false
	var errs []error
	for _, authority := range authorities {
		if authority == nil {
			continue
		}
		protected, err := authority.VerifyProcessStart(evidence, executable)
		matched = matched || protected
		errs = append(errs, err)
	}
	if !matched {
		errs = append(errs, fmt.Errorf("executable %s has no retained byte authority", executable))
	}
	return errors.Join(errs...)
}
