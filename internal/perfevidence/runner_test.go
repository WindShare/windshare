package perfevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestApplicationRunsDirectBenchmarksAndAggregatesDiagnostics(t *testing.T) {
	repository := makeRepository(t)
	commands := &scriptedCommandRunner{responses: []scriptedResponse{
		{output: readyDiskOutput(100)},
		{output: readyDiskOutput(200)},
	}}
	logger := &recordingLogger{}
	application := Application{
		Commands: commands,
		Logger:   logger,
		Now:      func() time.Time { return time.Unix(100, 0) },
		NewRunID: func() (string, error) { return "run-123", nil },
	}
	report, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository,
		SampleCount:    2,
		WorkloadIDs:    []string{"ready-real-disk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Kind != ReportKind || report.SchemaVersion != ReportSchemaVersion ||
		report.RunID != "run-123" || report.Status != OutcomeSucceeded {
		t.Fatalf("report identity = %+v", report)
	}
	if len(report.Workloads) != 1 || len(report.Workloads[0].Samples) != 2 {
		t.Fatalf("workload diagnostics = %+v", report.Workloads)
	}
	if got := aggregateMetric(report.Workloads[0].Aggregates, "BenchmarkReadyRealDisk/path_state=fresh", "ns/op"); got.Minimum != 100 || got.P50 != 100 || got.P95 != 200 || got.Maximum != 200 {
		t.Fatalf("duration aggregate = %+v", got)
	}
	if !OraclesPassed(report.Workloads[0].Oracles) {
		t.Fatalf("oracles = %+v", report.Workloads[0].Oracles)
	}
	assertDirectCommand(t, commands.commands[0], filepath.Join(repository, "core"))
	if len(logger.events) < 6 {
		t.Fatalf("events = %+v", logger.events)
	}
	benchmarkEvent := findEvent(logger.events, "benchmark-finished", "ready-real-disk-sample-01")
	if benchmarkEvent.Scenario != "ready-real-disk" || benchmarkEvent.WorkloadID != "ready-real-disk" ||
		benchmarkEvent.ExitCode == nil || *benchmarkEvent.ExitCode != 0 {
		t.Fatalf("benchmark event = %+v", benchmarkEvent)
	}
}

func TestApplicationReportsCommandFailureAndKeepsOutputTail(t *testing.T) {
	repository := makeRepository(t)
	commands := &scriptedCommandRunner{responses: []scriptedResponse{{
		output: "compile failed: missing symbol", exitCode: 1, err: errors.New("exit status 1"),
	}}}
	application := Application{
		Commands: commands,
		NewRunID: func() (string, error) { return "failed-run", nil },
	}
	report, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository,
		SampleCount:    1,
		WorkloadIDs:    []string{"relay-registration-wire"},
	})
	if err == nil || report.Status != OutcomeFailed || !strings.Contains(report.Error, "exit status 1") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	sample := report.Workloads[0].Samples[0]
	if sample.Status != OutcomeFailed || sample.Command.Outcome != OutcomeFailed ||
		sample.Command.OutputTail != "compile failed: missing symbol" {
		t.Fatalf("failed sample = %+v", sample)
	}
}

func TestApplicationPreservesHardOracleFailures(t *testing.T) {
	repository := makeRepository(t)
	commands := &scriptedCommandRunner{responses: []scriptedResponse{{output: relayOutput(4, 2)}}}
	application := Application{
		Commands: commands,
		NewRunID: func() (string, error) { return "oracle-run", nil },
	}
	report, err := application.Run(context.Background(), RunConfig{
		RepositoryRoot: repository,
		SampleCount:    1,
		WorkloadIDs:    []string{"relay-registration-wire"},
	})
	if err == nil || report.Status != OutcomeFailed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	results := report.Workloads[0].Oracles
	if len(results) != 3 || results[1].ID != "registration-write-count" || results[1].Passed {
		t.Fatalf("oracle results = %+v", results)
	}
}

func TestApplicationValidatesConfigurationBeforeLaunchingCommands(t *testing.T) {
	repository := makeRepository(t)
	commands := &scriptedCommandRunner{}
	application := Application{Commands: commands}
	tests := []RunConfig{
		{RepositoryRoot: repository, SampleCount: 0},
		{RepositoryRoot: repository, SampleCount: 1, WorkloadIDs: []string{"missing"}},
		{RepositoryRoot: filepath.Join(repository, "core"), SampleCount: 1},
	}
	for _, config := range tests {
		if _, err := application.Run(context.Background(), config); err == nil {
			t.Fatalf("config %+v was accepted", config)
		}
	}
	if len(commands.commands) != 0 {
		t.Fatalf("invalid configuration launched commands: %+v", commands.commands)
	}
}

func TestReplaceEnvironmentUsesOneLocalToolchainAssignment(t *testing.T) {
	environment := replaceEnvironment(
		[]string{"PATH=value", "gotoolchain=auto", "GOTOOLCHAIN=remote", "OTHER=kept"},
		localToolchainEnvironment,
		"local",
	)
	if !slices.Equal(environment, []string{"PATH=value", "GOTOOLCHAIN=local", "OTHER=kept"}) {
		t.Fatalf("environment = %q", environment)
	}
}

func TestResolveRepositoryRootAcceptsExplicitWindShareRoot(t *testing.T) {
	repository := makeRepository(t)
	resolved, err := resolveRepositoryRoot(repository)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Clean(repository) {
		t.Fatalf("resolved = %q, want %q", resolved, repository)
	}
}

func assertDirectCommand(t *testing.T, command Command, wantDirectory string) {
	t.Helper()
	wantArguments := []string{
		"test", "-run", "^$", "-bench", "^BenchmarkReadyRealDisk$", "-benchmem",
		"-benchtime=20x", "-count=1", "-timeout=15m0s", "./liveshare",
	}
	if command.Executable != "go" || command.Directory != wantDirectory ||
		!slices.Equal(command.Arguments, wantArguments) {
		t.Fatalf("direct command = %+v", command)
	}
}

func aggregateMetric(aggregates []BenchmarkAggregate, benchmark, metric string) BenchmarkAggregate {
	for _, aggregate := range aggregates {
		if aggregate.Benchmark == benchmark && aggregate.Metric == metric {
			return aggregate
		}
	}
	return BenchmarkAggregate{}
}

func findEvent(events []Event, milestone, operationID string) Event {
	for _, event := range events {
		if event.Milestone == milestone && event.OperationID == operationID {
			return event
		}
	}
	return Event{}
}

func readyDiskOutput(nsPerOperation int) string {
	return strings.Join([]string{
		"goos: windows",
		"BenchmarkReadyRealDisk/path_state=fresh-16 20 " + benchmarkMetrics(nsPerOperation, "42 registration-material-bytes/op"),
		"BenchmarkReadyRealDisk/path_state=reused-16 20 " + benchmarkMetrics(nsPerOperation+10, "42 registration-material-bytes/op"),
		"PASS",
	}, "\n")
}

func relayOutput(writes, reads int) string {
	extra := strings.Join([]string{
		"128 registration-wire-sent-B/op", "64 registration-wire-received-B/op",
		"32 descriptor-bytes/op", integerMetric(writes, "registration-writes/op"),
		integerMetric(reads, "registration-reads/op"),
	}, " ")
	return "BenchmarkRelaySenderRegistration-16 100 " + benchmarkMetrics(100, extra) + "\nPASS\n"
}

func benchmarkMetrics(nsPerOperation int, extra string) string {
	return integerMetric(nsPerOperation, "ns/op") + " 8 B/op 1 allocs/op " + extra
}

func integerMetric(value int, unit string) string {
	return fmt.Sprintf("%d %s", value, unit)
}

func makeRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, relative := range []string{"go.work", "go.mod", "core/go.mod"} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("module test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type scriptedResponse struct {
	output   string
	exitCode int
	err      error
}

type scriptedCommandRunner struct {
	commands  []Command
	responses []scriptedResponse
}

func (runner *scriptedCommandRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	index := len(runner.commands)
	runner.commands = append(runner.commands, command)
	if index >= len(runner.responses) {
		return CommandResult{ExitCode: -1}, errors.New("unexpected command")
	}
	response := runner.responses[index]
	startedAt := time.Unix(int64(index+1), 0)
	return CommandResult{
		Output: []byte(response.output), ProcessID: index + 100, ExitCode: response.exitCode,
		StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
	}, response.err
}

type recordingLogger struct {
	events []Event
}

func (logger *recordingLogger) Log(event Event) error {
	logger.events = append(logger.events, event)
	return nil
}
