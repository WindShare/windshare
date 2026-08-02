package perfevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/internal/perfevidence/processrun"
	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	maximumCommandOutputBytes            = 32 << 20
	maximumBinaryBytes                   = 512 << 20
	maximumProfileBytes                  = 1 << 30
	maximumProtectedOutputBytes          = maximumProfileBytes
	maximumProtectedOutputAggregateBytes = 2 * maximumProfileBytes
	// Profile capture is the widest artifact-producing command: one CPU and one
	// memory profile. A larger group cannot describe any supported operation.
	maximumProtectedOutputCount           = 2
	protectedOutputAbortSettlementTimeout = 5 * time.Second
)

type MutationRoot struct {
	HostPath string
	Name     string
}

type MutationDomainSpec struct {
	RuntimeRoot string
	Roots       []MutationRoot
}

type MutationDomainCommand struct {
	Executable   string
	Arguments    []string
	Directory    string
	Environment  []string
	Outputs      []MutationOutput
	RestorePaths bool
}

type MutationOutput struct {
	HostPath string
	MaxBytes int64
}

type mutationIntent uint8

const (
	mutationIntentNone mutationIntent = iota
	mutationIntentVerification
	mutationIntentArtifactProduction
)

type MutationDomainResult struct {
	Stdout     []byte
	Stderr     []byte
	ProcessID  int
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

type MutationOutputSink interface {
	// Implementations must treat cancellation as a settlement request: a method
	// may not return until it can prove that no late write, seal, or group adoption can occur.
	WriteContext(context.Context, []byte) (int, error)
	// Seal fixes one output against its framed byte identity. Publication is a
	// separate group transition so a later output failure can still roll it back.
	Seal(context.Context, int64, string) error
	Abort(context.Context) error
}

type mutationOutputSink interface {
	MutationOutputSink
	adopt() (byteConsumptionAuthority, error)
	finalize()
}

// MutationDomain is defined at its consumer boundary so the measurement
// runner remains testable without importing an OS isolation implementation.
type MutationDomain interface {
	Run(context.Context, MutationDomainCommand, map[string]MutationOutputSink) (MutationDomainResult, error)
	Close() error
}

type MutationDomainFactory interface {
	Open(context.Context, MutationDomainSpec) (MutationDomain, error)
}

type Command struct {
	Executable  string
	Arguments   []string
	Directory   string
	Environment []string
	// ReplaceEnvironment is required for provenance-sensitive commands. Merely
	// appending overrides leaves GOENV/GOFLAGS and platform aliases able to alter
	// the build before the explicit values are interpreted.
	ReplaceEnvironment bool
	authorities        []byteConsumptionAuthority
	mutationIntent     mutationIntent
	protectedOutputs   []MutationOutput
	restorePaths       bool
}

type CommandResult struct {
	Output            []byte
	Stdout            []byte
	Stderr            []byte
	ProcessID         int
	ExitCode          int
	StartedAt         time.Time
	FinishedAt        time.Time
	outputAuthorities map[string]byteConsumptionAuthority
}

type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
}

type OwnedCommandRunner interface {
	Run(context.Context, processrun.Spec) (processrun.Result, error)
}

type ProcessRunner struct {
	Now                         func() time.Time
	MutationDomain              MutationDomain
	OwnedCommands               OwnedCommandRunner
	prepareOutput               func(string) (mutationOutputSink, error)
	protectedOutputAbortTimeout time.Duration
}

func (runner ProcessRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	switch command.mutationIntent {
	case mutationIntentVerification, mutationIntentArtifactProduction:
		return runner.runPrivateMutation(ctx, command)
	case mutationIntentNone:
	default:
		return CommandResult{ExitCode: -1}, errors.New("command mutation intent is unsupported")
	}
	result := CommandResult{ExitCode: -1}
	if ctx == nil {
		return result, errors.New("command context is nil")
	}
	now := runner.Now
	if now == nil {
		now = time.Now
	}
	if err := verifyConsumptionAuthorities(command.authorities); err != nil {
		result.FinishedAt = now().UTC()
		return result, fmt.Errorf("verify command byte authority before launch: %w", err)
	}
	executable, directory, environment, err := prepareOwnedCommand(command)
	if err != nil {
		result.FinishedAt = now().UTC()
		return result, errors.Join(err, verifyConsumptionAuthorities(command.authorities))
	}
	identity, err := processrun.NewIdentity()
	if err != nil {
		result.FinishedAt = now().UTC()
		return result, errors.Join(err, verifyConsumptionAuthorities(command.authorities))
	}
	deadline, err := ownedCommandDeadline(ctx)
	if err != nil {
		result.FinishedAt = now().UTC()
		return result, errors.Join(err, verifyConsumptionAuthorities(command.authorities))
	}
	ownedRunner := runner.OwnedCommands
	if ownedRunner == nil {
		ownedRunner = processrun.Runner{MaximumOutput: maximumCommandOutputBytes}
	}
	result.StartedAt = now().UTC()
	owned, runErr := ownedRunner.Run(ctx, processrun.Spec{
		Identity:         identity,
		Executable:       executable,
		Arguments:        append([]string(nil), command.Arguments...),
		WorkingDirectory: directory,
		Environment:      environment,
		Deadline:         deadline,
		TerminationGrace: processrun.DefaultTerminationGrace,
		AuthorizeStart: func(evidence protocol.StartEvidence) error {
			return verifyOwnedProcessStart(command.authorities, evidence, executable)
		},
	})
	result.FinishedAt = now().UTC()
	result.Stdout = append([]byte(nil), owned.Stdout...)
	result.Stderr = append([]byte(nil), owned.Stderr...)
	result.Output = append(append([]byte(nil), result.Stdout...), result.Stderr...)
	result.ProcessID = owned.ProcessID
	result.ExitCode = owned.ExitCode
	return result, errors.Join(runErr, verifyConsumptionAuthorities(command.authorities))
}

func prepareOwnedCommand(command Command) (
	string,
	string,
	[]protocol.EnvironmentEntry,
	error,
) {
	executable, err := exec.LookPath(command.Executable)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command executable path: %w", err)
	}
	executable = filepath.Clean(executable)
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", nil, errors.Join(
			errors.New("owned command executable is not a regular file"),
			err,
		)
	}
	directory := command.Directory
	if directory == "" {
		directory, err = os.Getwd()
	} else {
		directory, err = filepath.Abs(directory)
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve owned command working directory: %w", err)
	}
	directory = filepath.Clean(directory)
	info, err = os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", "", nil, errors.Join(
			errors.New("owned command working directory is not a directory"),
			err,
		)
	}
	var base []string
	if !command.ReplaceEnvironment {
		base = os.Environ()
	}
	environment, err := processrun.CanonicalEnvironment(base, command.Environment)
	if err != nil {
		return "", "", nil, fmt.Errorf("canonicalize owned command environment: %w", err)
	}
	return executable, directory, environment, nil
}

func ownedCommandDeadline(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	deadline := processrun.DefaultCommandDeadline
	if contextDeadline, present := ctx.Deadline(); present {
		remaining := time.Until(contextDeadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		remaining = ((remaining + time.Millisecond - 1) / time.Millisecond) * time.Millisecond
		if remaining < deadline {
			deadline = remaining
		}
	}
	maximum := time.Duration(protocol.MaximumDeadlineMilliseconds) * time.Millisecond
	if deadline > maximum {
		deadline = maximum
	}
	return deadline, nil
}

func (runner ProcessRunner) runPrivateMutation(ctx context.Context, command Command) (
	result CommandResult,
	resultErr error,
) {
	if runner.MutationDomain == nil {
		return CommandResult{ExitCode: -1}, errors.New("provenance-sensitive command has no private mutation domain")
	}
	if ctx == nil {
		return CommandResult{ExitCode: -1}, errors.New("command context is nil")
	}
	if err := verifyConsumptionAuthorities(command.authorities); err != nil {
		return CommandResult{ExitCode: -1}, fmt.Errorf("verify command byte authority before isolation: %w", err)
	}
	if err := validateMutationOutputs(command.mutationIntent, command.protectedOutputs); err != nil {
		return CommandResult{ExitCode: -1}, fmt.Errorf("validate protected command outputs: %w", err)
	}
	prepareOutput := runner.prepareOutput
	if prepareOutput == nil {
		prepareOutput = prepareMutationOutput
	}
	sinks := make(map[string]MutationOutputSink, len(command.protectedOutputs))
	prepared := make([]preparedMutationOutput, 0, len(command.protectedOutputs))
	for _, output := range command.protectedOutputs {
		sink, err := prepareOutput(output.HostPath)
		if err != nil {
			resultErr = fmt.Errorf("prepare protected command output %s: %w", output.HostPath, err)
			break
		}
		sinks[output.HostPath] = sink
		prepared = append(prepared, preparedMutationOutput{path: output.HostPath, sink: sink})
	}
	defer func() {
		abortTimeout := runner.protectedOutputAbortTimeout
		if abortTimeout <= 0 {
			abortTimeout = protectedOutputAbortSettlementTimeout
		}
		abortContext, cancel := context.WithTimeout(context.Background(), abortTimeout)
		defer cancel()
		for index := len(prepared) - 1; index >= 0; index-- {
			resultErr = errors.Join(resultErr, prepared[index].sink.Abort(abortContext))
		}
	}()
	if resultErr != nil {
		return CommandResult{ExitCode: -1}, resultErr
	}
	isolated, err := runner.MutationDomain.Run(ctx, MutationDomainCommand{
		Executable: command.Executable, Arguments: append([]string(nil), command.Arguments...),
		Directory: command.Directory, Environment: append([]string(nil), command.Environment...),
		Outputs:      append([]MutationOutput(nil), command.protectedOutputs...),
		RestorePaths: command.restorePaths,
	}, sinks)
	result = CommandResult{
		Stdout: isolated.Stdout, Stderr: isolated.Stderr, ProcessID: isolated.ProcessID,
		ExitCode: isolated.ExitCode, StartedAt: isolated.StartedAt, FinishedAt: isolated.FinishedAt,
	}
	result.Output = append(append([]byte(nil), result.Stdout...), result.Stderr...)
	if err == nil {
		err = verifyConsumptionAuthorities(command.authorities)
	} else {
		err = errors.Join(err, verifyConsumptionAuthorities(command.authorities))
	}
	if err == nil && result.ExitCode == 0 && command.mutationIntent == mutationIntentArtifactProduction {
		result.outputAuthorities, err = adoptMutationOutputGroup(prepared)
	}
	return result, err
}

type preparedMutationOutput struct {
	path string
	sink mutationOutputSink
}

func validateMutationOutputs(intent mutationIntent, outputs []MutationOutput) error {
	switch intent {
	case mutationIntentVerification:
		if len(outputs) != 0 {
			return errors.New("private verification command cannot publish protected outputs")
		}
		return nil
	case mutationIntentArtifactProduction:
		if len(outputs) == 0 {
			return errors.New("artifact-producing mutation requires at least one protected output")
		}
	default:
		return errors.New("private command mutation intent is unsupported")
	}
	if len(outputs) > maximumProtectedOutputCount {
		return fmt.Errorf(
			"protected output count must be in [1, %d]",
			maximumProtectedOutputCount,
		)
	}
	seen := make(map[string]string, len(outputs))
	var aggregate int64
	for _, output := range outputs {
		if output.HostPath == "" || !filepath.IsAbs(output.HostPath) || filepath.Clean(output.HostPath) != output.HostPath {
			return fmt.Errorf("protected output path %q is not canonical and absolute", output.HostPath)
		}
		if output.MaxBytes < 1 || output.MaxBytes > maximumProtectedOutputBytes {
			return fmt.Errorf(
				"protected output %s max bytes must be in [1, %d]",
				output.HostPath,
				maximumProtectedOutputBytes,
			)
		}
		if aggregate > int64(maximumProtectedOutputAggregateBytes)-output.MaxBytes {
			return fmt.Errorf(
				"protected outputs exceed aggregate byte limit %d",
				maximumProtectedOutputAggregateBytes,
			)
		}
		aggregate += output.MaxBytes

		parent, err := filepath.EvalSymlinks(filepath.Dir(output.HostPath))
		if err != nil {
			return fmt.Errorf("resolve protected output parent %s: %w", output.HostPath, err)
		}
		parentInfo, err := os.Stat(parent)
		if err != nil || !parentInfo.IsDir() {
			return errors.Join(fmt.Errorf("protected output parent %s is not a directory", parent), err)
		}
		key := platformPathKey(filepath.Join(parent, filepath.Base(output.HostPath)))
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("protected output path %s aliases duplicate %s", output.HostPath, previous)
		}
		seen[key] = output.HostPath
	}
	return nil
}

func adoptMutationOutputGroup(
	prepared []preparedMutationOutput,
) (map[string]byteConsumptionAuthority, error) {
	authorities := make(map[string]byteConsumptionAuthority, len(prepared))
	ordered := make([]byteConsumptionAuthority, 0, len(prepared))
	for _, output := range prepared {
		authority, err := output.sink.adopt()
		if err != nil {
			return nil, fmt.Errorf("adopt protected command output %s: %w", output.path, err)
		}
		if authority == nil {
			return nil, fmt.Errorf("adopt protected command output %s: retained authority is unavailable", output.path)
		}
		authorities[output.path] = authority
		ordered = append(ordered, authority)
	}
	if err := verifyConsumptionAuthorities(ordered); err != nil {
		return nil, fmt.Errorf("verify protected output group before publication: %w", err)
	}
	// finalize is deliberately infallible: every OS operation that can fail has
	// already completed while all members are still rollback-capable.
	for _, output := range prepared {
		output.sink.finalize()
	}
	return authorities, nil
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
