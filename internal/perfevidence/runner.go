package perfevidence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	benchmarkCommandTimeout       = 15 * time.Minute
	maximumCommandDiagnosticBytes = 8 << 10
	performanceComponent          = "perfevidence"
	performanceRunnerScenario     = "performance-runner"
)

type RunConfig struct {
	RepositoryRoot string
	SampleCount    int
	WorkloadIDs    []string
}

type Application struct {
	Commands CommandRunner
	Logger   EventLogger
	Now      func() time.Time
	NewRunID func() (string, error)
}

func (application Application) Run(ctx context.Context, config RunConfig) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("performance runner context is nil")
	}
	if application.Commands == nil {
		return Report{}, errors.New("performance runner requires a command runner")
	}
	if config.SampleCount < 1 {
		return Report{}, errors.New("sample count must be positive")
	}
	repositoryRoot, err := resolveRepositoryRoot(config.RepositoryRoot)
	if err != nil {
		return Report{}, err
	}
	workloads, err := SelectWorkloads(config.WorkloadIDs)
	if err != nil {
		return Report{}, err
	}
	runID, err := application.generateRunID()
	if err != nil {
		return Report{}, fmt.Errorf("create performance run ID: %w", err)
	}
	report := application.newReport(runID, config.SampleCount)
	if err := application.log(Event{
		RunID: runID, OperationID: runID, Scenario: performanceRunnerScenario,
		Milestone: "run-started", Outcome: "running",
	}); err != nil {
		return application.failReport(report, fmt.Errorf("log performance run start: %w", err))
	}
	runner := benchmarkRunner{
		commands: application.Commands, logger: application.Logger,
		now: application.now(), runID: runID, repositoryRoot: repositoryRoot,
	}
	var failures []error
	for _, workload := range workloads {
		if err := ctx.Err(); err != nil {
			failures = append(failures, context.Cause(ctx))
			break
		}
		result, measureErr := runner.measure(ctx, workload, config.SampleCount)
		report.Workloads = append(report.Workloads, result)
		if measureErr != nil {
			failures = append(failures, fmt.Errorf("measure workload %s: %w", workload.ID, measureErr))
		}
	}
	runErr := errors.Join(failures...)
	report.FinishedAt = application.now()().UTC()
	report.Status = outcomeFor(runErr)
	if runErr != nil {
		report.Error = runErr.Error()
	}
	logErr := application.log(Event{
		RunID: runID, OperationID: runID, Scenario: performanceRunnerScenario,
		Milestone: "run-finished", Outcome: string(report.Status), Detail: report.Error,
	})
	if logErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("log performance run completion: %w", logErr))
		report.Status = OutcomeFailed
		report.Error = runErr.Error()
	}
	return report, runErr
}

func (application Application) newReport(runID string, sampleCount int) Report {
	return Report{
		SchemaVersion:      ReportSchemaVersion,
		Kind:               ReportKind,
		RunID:              runID,
		Status:             OutcomeSucceeded,
		StartedAt:          application.now()().UTC(),
		SamplesPerWorkload: sampleCount,
		Environment: EnvironmentContext{
			OS: runtime.GOOS, Architecture: runtime.GOARCH,
			LogicalProcessors: runtime.NumCPU(), GoVersion: runtime.Version(),
		},
	}
}

func (application Application) failReport(report Report, err error) (Report, error) {
	report.Status = OutcomeFailed
	report.Error = err.Error()
	report.FinishedAt = application.now()().UTC()
	return report, err
}

func (application Application) now() func() time.Time {
	if application.Now != nil {
		return application.Now
	}
	return time.Now
}

func (application Application) generateRunID() (string, error) {
	if application.NewRunID == nil {
		return randomRunID()
	}
	return application.NewRunID()
}

func (application Application) log(event Event) error {
	if application.Logger == nil {
		return nil
	}
	event.Timestamp = application.now()().UTC()
	event.Component = performanceComponent
	return application.Logger.Log(event)
}

type benchmarkRunner struct {
	commands       CommandRunner
	logger         EventLogger
	now            func() time.Time
	runID          string
	repositoryRoot string
}

func (runner benchmarkRunner) measure(
	ctx context.Context,
	workload Workload,
	sampleCount int,
) (WorkloadResult, error) {
	result := WorkloadResult{Definition: workload}
	if err := runner.log(Event{
		OperationID: workload.ID, Scenario: workload.ID, WorkloadID: workload.ID,
		Milestone: "workload-started", Outcome: "running",
	}); err != nil {
		return runner.completeWorkload(result, fmt.Errorf("log workload start: %w", err))
	}
	for sampleIndex := 1; sampleIndex <= sampleCount; sampleIndex++ {
		sample, err := runner.measureSample(ctx, workload, sampleIndex)
		result.Samples = append(result.Samples, sample)
		if err != nil {
			return runner.completeWorkload(result, err)
		}
	}
	aggregates, err := AggregateSamples(result.Samples, sampleCount)
	if err != nil {
		return runner.completeWorkload(result, err)
	}
	result.Aggregates = aggregates
	result.Oracles = EvaluateOracles(workload, result.Samples)
	if !OraclesPassed(result.Oracles) {
		return runner.completeWorkload(result, fmt.Errorf("workload %s failed a hard oracle", workload.ID))
	}
	return runner.completeWorkload(result, nil)
}

func (runner benchmarkRunner) measureSample(
	ctx context.Context,
	workload Workload,
	sampleIndex int,
) (BenchmarkSample, error) {
	operationID := fmt.Sprintf("%s-sample-%02d", workload.ID, sampleIndex)
	command := benchmarkCommand(runner.repositoryRoot, workload)
	sample := BenchmarkSample{
		WorkloadID: workload.ID, Index: sampleIndex,
		Command: commandDiagnostic(
			operationID, workload.ModuleDir, command,
			CommandResult{ExitCode: -1}, errors.New("command was not started"),
		),
	}
	if err := runner.log(Event{
		OperationID: operationID, Scenario: workload.ID, WorkloadID: workload.ID, SampleIndex: sampleIndex,
		Milestone: "benchmark-started", Outcome: "running",
	}); err != nil {
		return runner.completeSample(sample, nil, fmt.Errorf("log benchmark start: %w", err))
	}
	commandContext, cancel := context.WithTimeout(ctx, benchmarkCommandTimeout)
	defer cancel()
	commandResult, runErr := runner.commands.Run(commandContext, command)
	sample.Command = commandDiagnostic(operationID, workload.ModuleDir, command, commandResult, runErr)
	if runErr != nil || commandResult.ExitCode != 0 {
		return runner.completeSample(sample, commandResult.Output, commandError(commandResult, runErr))
	}
	rows, err := ParseBenchmarkOutput(commandResult.Output)
	if err != nil {
		return runner.completeSample(sample, commandResult.Output, fmt.Errorf("parse benchmark output: %w", err))
	}
	if err := ValidateSample(workload, rows); err != nil {
		return runner.completeSample(sample, commandResult.Output, fmt.Errorf("validate benchmark output: %w", err))
	}
	sample.Rows = rows
	return runner.completeSample(sample, nil, nil)
}

func (runner benchmarkRunner) completeSample(
	sample BenchmarkSample,
	output []byte,
	sampleErr error,
) (BenchmarkSample, error) {
	sample.Status = outcomeFor(sampleErr)
	if sampleErr != nil {
		sample.Error = sampleErr.Error()
		sample.Command.OutputTail = diagnosticTail(output)
	}
	exitCode := sample.Command.ExitCode
	duration := max(sample.Command.FinishedAt.Sub(sample.Command.StartedAt), 0)
	logErr := runner.log(Event{
		OperationID: sample.Command.OperationID, Scenario: sample.WorkloadID,
		WorkloadID: sample.WorkloadID, SampleIndex: sample.Index,
		Milestone: "benchmark-finished", Outcome: string(sample.Status),
		ExitCode: &exitCode, DurationMillis: duration.Milliseconds(), Detail: sample.Error,
	})
	if logErr != nil {
		sampleErr = errors.Join(sampleErr, fmt.Errorf("log benchmark completion: %w", logErr))
		sample.Status = OutcomeFailed
		sample.Error = sampleErr.Error()
	}
	return sample, sampleErr
}

func (runner benchmarkRunner) completeWorkload(
	result WorkloadResult,
	workloadErr error,
) (WorkloadResult, error) {
	result.Status = outcomeFor(workloadErr)
	if workloadErr != nil {
		result.Error = workloadErr.Error()
	}
	logErr := runner.log(Event{
		OperationID: result.Definition.ID, Scenario: result.Definition.ID,
		WorkloadID: result.Definition.ID, Milestone: "workload-finished",
		Outcome: string(result.Status), Detail: result.Error,
	})
	if logErr != nil {
		workloadErr = errors.Join(workloadErr, fmt.Errorf("log workload completion: %w", logErr))
		result.Status = OutcomeFailed
		result.Error = workloadErr.Error()
	}
	return result, workloadErr
}

func (runner benchmarkRunner) log(event Event) error {
	if runner.logger == nil {
		return nil
	}
	event.Timestamp = runner.now().UTC()
	event.RunID = runner.runID
	event.Component = performanceComponent
	return runner.logger.Log(event)
}

func benchmarkCommand(repositoryRoot string, workload Workload) Command {
	return Command{
		Executable: "go",
		Arguments: []string{
			"test", "-run", "^$", "-bench", "^" + regexp.QuoteMeta(workload.Benchmark) + "$",
			"-benchmem", "-benchtime=" + workload.BenchTime, "-count=1",
			"-timeout=" + benchmarkCommandTimeout.String(), workload.Package,
		},
		Directory: filepath.Join(repositoryRoot, filepath.FromSlash(workload.ModuleDir)),
	}
}

func commandDiagnostic(
	operationID string,
	moduleDir string,
	command Command,
	result CommandResult,
	runErr error,
) CommandDiagnostic {
	executionErr := commandExecutionError(result, runErr)
	diagnostic := CommandDiagnostic{
		OperationID: operationID, Executable: command.Executable,
		Arguments: append([]string(nil), command.Arguments...), Directory: filepath.ToSlash(moduleDir),
		ProcessID: result.ProcessID, ExitCode: result.ExitCode,
		StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
		Outcome: outcomeFor(executionErr),
	}
	if executionErr != nil {
		diagnostic.Error = executionErr.Error()
		diagnostic.OutputTail = diagnosticTail(result.Output)
	}
	return diagnostic
}

func commandExecutionError(result CommandResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("command exited with %d", result.ExitCode)
	}
	return nil
}

func commandError(result CommandResult, runErr error) error {
	err := commandExecutionError(result, runErr)
	if err == nil {
		return errors.New("benchmark command failed without an execution diagnostic")
	}
	return fmt.Errorf("run benchmark command: %w", err)
}

func diagnosticTail(output []byte) string {
	if len(output) > maximumCommandDiagnosticBytes {
		output = output[len(output)-maximumCommandDiagnosticBytes:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(output), "�"))
}

func outcomeFor(err error) Outcome {
	if err == nil {
		return OutcomeSucceeded
	}
	return OutcomeFailed
}

func randomRunID() (string, error) {
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return "", err
	}
	return hex.EncodeToString(identifier), nil
}

func resolveRepositoryRoot(requested string) (string, error) {
	if requested != "" {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			return "", fmt.Errorf("resolve repository root: %w", err)
		}
		if !isRepositoryRoot(absolute) {
			return "", fmt.Errorf("%s is not the WindShare repository root", absolute)
		}
		return filepath.Clean(absolute), nil
	}
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("read working directory: %w", err)
	}
	for {
		if isRepositoryRoot(current) {
			return filepath.Clean(current), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("could not find the WindShare repository root")
		}
		current = parent
	}
}

func isRepositoryRoot(path string) bool {
	for _, relative := range []string{"go.work", "go.mod", "core/go.mod"} {
		info, err := os.Stat(filepath.Join(path, filepath.FromSlash(relative)))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}
