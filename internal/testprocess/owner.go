// Package testprocess launches correctness-test children through a bounded,
// platform-native process-tree owner.
package testprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	helperCommandPackage = "./cmd/testprocessowner"
	helperSelfCheck      = "testprocessowner ready\n"
	maximumBuildOutput   = 64 << 10
)

type Command struct {
	Executable       string
	Arguments        []string
	WorkingDirectory string
	Environment      []string
}

type Spec struct {
	Identity         testrun.Identity
	Command          Command
	Deadline         time.Duration
	TerminationGrace time.Duration
}

type Owner struct {
	helperPath     string
	ownedDirectory string

	mu       sync.Mutex
	active   int
	closed   bool
	closeErr error
}

func NewOwner(helperPath string) (*Owner, error) {
	if !filepath.IsAbs(helperPath) || filepath.Clean(helperPath) != helperPath {
		return nil, errors.New("process-owner helper path must be absolute and canonical")
	}
	metadata, err := os.Stat(helperPath)
	if err != nil {
		return nil, fmt.Errorf("inspect process-owner helper: %w", err)
	}
	if !metadata.Mode().IsRegular() {
		return nil, errors.New("process-owner helper must be a regular file")
	}
	return &Owner{helperPath: helperPath}, nil
}

func BuildOwner(ctx context.Context, repositoryRoot string) (_ *Owner, resultErr error) {
	if !filepath.IsAbs(repositoryRoot) || filepath.Clean(repositoryRoot) != repositoryRoot {
		return nil, errors.New("repository root must be absolute and canonical")
	}
	directory, err := os.MkdirTemp("", "windshare-testprocessowner-")
	if err != nil {
		return nil, fmt.Errorf("create process-owner build directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, os.RemoveAll(directory))
		}
	}()
	name := "testprocessowner"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	helperPath := filepath.Join(directory, name)
	command := exec.CommandContext(ctx, goExecutable(), "build", "-trimpath", "-o", helperPath, helperCommandPackage)
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("build process-owner helper: %w: %s", err, boundedBuildOutput(output))
	}
	owner, err := NewOwner(helperPath)
	if err != nil {
		return nil, err
	}
	if err := owner.SelfCheck(ctx); err != nil {
		return nil, err
	}
	owner.ownedDirectory = directory
	keep = true
	return owner, nil
}

func goExecutable() string {
	if executable := os.Getenv("WINDSHARE_GO_EXECUTABLE"); executable != "" {
		return executable
	}
	return "go"
}

func boundedBuildOutput(output []byte) string {
	if len(output) > maximumBuildOutput {
		output = output[len(output)-maximumBuildOutput:]
	}
	return string(output)
}

func (owner *Owner) SelfCheck(ctx context.Context) error {
	if owner == nil {
		return errors.New("process owner is nil")
	}
	command := exec.CommandContext(ctx, owner.helperPath, "self-check")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("run process-owner self-check: %w: %s", err, boundedBuildOutput(output.Bytes()))
	}
	if output.String() != helperSelfCheck {
		return fmt.Errorf("unexpected process-owner self-check output %q", output.String())
	}
	return nil
}

func (owner *Owner) Start(ctx context.Context, spec Spec) (*Process, error) {
	if owner == nil {
		return nil, errors.New("process owner is nil")
	}
	owner.mu.Lock()
	if owner.closed {
		owner.mu.Unlock()
		return nil, errors.New("process owner is closed")
	}
	owner.active++
	owner.mu.Unlock()
	releaseOnce := sync.Once{}
	release := func() {
		releaseOnce.Do(func() {
			owner.mu.Lock()
			owner.active--
			owner.mu.Unlock()
		})
	}
	process, err := startProcess(ctx, owner.helperPath, spec, release)
	if err != nil {
		release()
		return nil, err
	}
	return process, nil
}

func (owner *Owner) Close() error {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.closed {
		return owner.closeErr
	}
	if owner.active != 0 {
		return fmt.Errorf("process owner still has %d active process trees", owner.active)
	}
	owner.closed = true
	if owner.ownedDirectory != "" {
		owner.closeErr = os.RemoveAll(owner.ownedDirectory)
	}
	return owner.closeErr
}

func configForSpec(spec Spec) (processowner.Config, error) {
	if err := testrun.ValidateIdentity(spec.Identity); err != nil {
		return processowner.Config{}, fmt.Errorf("test process identity: %w", err)
	}
	operation, err := testrun.NewOperation(spec.Identity.RunID, spec.Identity.OperationID, spec.Identity.Scenario)
	if err != nil {
		return processowner.Config{}, err
	}
	environment, err := operation.ChildEnvironment(spec.Command.Environment)
	if err != nil {
		return processowner.Config{}, err
	}
	config := processowner.Config{
		Executable: spec.Command.Executable, Arguments: append([]string(nil), spec.Command.Arguments...),
		WorkingDirectory: spec.Command.WorkingDirectory, Environment: environment,
		DeadlineMilliseconds:         spec.Deadline.Milliseconds(),
		TerminationGraceMilliseconds: spec.TerminationGrace.Milliseconds(),
	}
	if err := processowner.ValidateConfig(config); err != nil {
		return processowner.Config{}, err
	}
	return config, nil
}
