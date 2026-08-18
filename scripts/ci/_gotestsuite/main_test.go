package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestRunDiscoversTheNamedSuiteBeforeExecutingIt(t *testing.T) {
	t.Parallel()

	executor := &recordingTestExecutor{tests: []string{"TestCriticalPath"}}
	err := run(
		context.Background(),
		[]string{"-run", "^TestCriticalPath$", "./e2e"},
		io.Discard,
		io.Discard,
		executor,
	)
	if err != nil {
		t.Fatalf("run named suite: %v", err)
	}
	want := namedTestSuite{packagePattern: "./e2e", testPattern: "^TestCriticalPath$"}
	if !reflect.DeepEqual(executor.listed, []namedTestSuite{want}) {
		t.Fatalf("listed suites = %#v, want %#v", executor.listed, []namedTestSuite{want})
	}
	if !reflect.DeepEqual(executor.executed, []namedTestSuite{want}) {
		t.Fatalf("executed suites = %#v, want %#v", executor.executed, []namedTestSuite{want})
	}
}

func TestRunRejectsAnEmptyNamedSuiteBeforeExecution(t *testing.T) {
	t.Parallel()

	executor := &recordingTestExecutor{}
	err := run(
		context.Background(),
		[]string{"-run", "^TestRenamedAway$", "./e2e"},
		io.Discard,
		io.Discard,
		executor,
	)
	if err == nil || !strings.Contains(err.Error(), "matched no top-level tests") {
		t.Fatalf("run() error = %v, want empty-suite failure", err)
	}
	if len(executor.executed) != 0 {
		t.Fatalf("executed suites = %#v, want none", executor.executed)
	}
}

func TestRunPropagatesDiscoveryAndExecutionFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		executor   *recordingTestExecutor
		wantDetail string
	}{
		{
			name:       "discovery",
			executor:   &recordingTestExecutor{listErr: errors.New("compile failed")},
			wantDetail: "discover tests",
		},
		{
			name:       "execution",
			executor:   &recordingTestExecutor{tests: []string{"TestLongPath"}, runErr: errors.New("test failed")},
			wantDetail: "run tests",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := run(
				context.Background(),
				[]string{"-run", "^TestLong", "./e2e"},
				io.Discard,
				io.Discard,
				test.executor,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("run() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestRunRequiresOnePatternAndOnePackage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantDetail string
	}{
		{name: "missing pattern", args: []string{"./e2e"}, wantDetail: "-run is required"},
		{name: "missing package", args: []string{"-run", "^TestLong"}, wantDetail: "exactly one package"},
		{name: "multiple packages", args: []string{"-run", "^TestLong", "./e2e", "./core/catalog"}, wantDetail: "exactly one package"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := run(
				context.Background(),
				test.args,
				io.Discard,
				io.Discard,
				&recordingTestExecutor{},
			)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("run() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestListedTopLevelTestsIgnoresOtherGoTargetsAndPackageStatus(t *testing.T) {
	t.Parallel()

	got, err := listedTopLevelTests([]byte(strings.Join([]string{
		"TestCriticalPath",
		"BenchmarkTransfer",
		"FuzzCapability",
		"ExampleLink",
		"ok  github.com/windshare/windshare/e2e  0.031s",
	}, "\n")))
	if err != nil {
		t.Fatalf("parse listing: %v", err)
	}
	want := []string{"TestCriticalPath"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed tests = %v, want %v", got, want)
	}
}

type recordingTestExecutor struct {
	tests    []string
	listErr  error
	runErr   error
	listed   []namedTestSuite
	executed []namedTestSuite
}

func (executor *recordingTestExecutor) list(_ context.Context, suite namedTestSuite) ([]string, error) {
	executor.listed = append(executor.listed, suite)
	return append([]string(nil), executor.tests...), executor.listErr
}

func (executor *recordingTestExecutor) run(
	_ context.Context,
	suite namedTestSuite,
	_ io.Writer,
	_ io.Writer,
) error {
	executor.executed = append(executor.executed, suite)
	return executor.runErr
}
