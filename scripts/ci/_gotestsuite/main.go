package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const testCountArgument = "-count=1"

type namedTestSuite struct {
	packagePattern string
	testPattern    string
}

type testExecutor interface {
	list(context.Context, namedTestSuite) ([]string, error)
	run(context.Context, namedTestSuite, io.Writer, io.Writer) error
}

type goTestExecutor struct {
	directory string
}

func main() {
	if err := run(
		context.Background(),
		os.Args[1:],
		os.Stdout,
		os.Stderr,
		goTestExecutor{directory: "."},
	); err != nil {
		fmt.Fprintf(os.Stderr, "gotestsuite: %v\n", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	executor testExecutor,
) error {
	flags := flag.NewFlagSet("gotestsuite", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	testPattern := flags.String("run", "", "regular expression selecting top-level tests")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *testPattern == "" {
		return fmt.Errorf("-run is required")
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("exactly one package pattern is required")
	}

	suite := namedTestSuite{
		packagePattern: flags.Arg(0),
		testPattern:    *testPattern,
	}
	tests, err := executor.list(ctx, suite)
	if err != nil {
		return fmt.Errorf("discover tests in %s: %w", suite.packagePattern, err)
	}
	// go test intentionally succeeds when -run selects nothing. Discovering the
	// suite first turns the CI selection contract into a fail-closed invariant.
	if len(tests) == 0 {
		return fmt.Errorf(
			"pattern %q matched no top-level tests in %s",
			suite.testPattern,
			suite.packagePattern,
		)
	}
	if err := executor.run(ctx, suite, stdout, stderr); err != nil {
		return fmt.Errorf("run tests in %s: %w", suite.packagePattern, err)
	}
	return nil
}

func (executor goTestExecutor) list(ctx context.Context, suite namedTestSuite) ([]string, error) {
	command := exec.CommandContext(
		ctx,
		"go",
		"test",
		testCountArgument,
		"-list",
		suite.testPattern,
		suite.packagePattern,
	)
	command.Dir = executor.directory
	var commandStderr bytes.Buffer
	command.Stderr = &commandStderr
	output, err := command.Output()
	if err != nil {
		return nil, commandFailure(err, commandStderr.String())
	}
	return listedTopLevelTests(output)
}

func (executor goTestExecutor) run(
	ctx context.Context,
	suite namedTestSuite,
	stdout io.Writer,
	stderr io.Writer,
) error {
	command := exec.CommandContext(
		ctx,
		"go",
		"test",
		testCountArgument,
		"-run",
		suite.testPattern,
		suite.packagePattern,
	)
	command.Dir = executor.directory
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func listedTopLevelTests(output []byte) ([]string, error) {
	var tests []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Test") && !strings.ContainsAny(line, " \t") {
			tests = append(tests, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read go test listing: %w", err)
	}
	return tests, nil
}

func commandFailure(err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}
