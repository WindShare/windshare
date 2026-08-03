// Package testprocess runs correctness-test children through the repository's
// sole external process-tree owner.
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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	goExecutableEnvironment = "WINDSHARE_GO_EXECUTABLE"
	helperCommandPackage    = "./cmd/testprocessowner"
	helperSelfCheck         = "{\"schema_version\":\"windshare.process-owner-self-check/v1\",\"component\":\"testprocessowner\",\"milestone\":\"self_check\",\"outcome\":\"ready\"}\n"
)

type Command struct {
	Executable       string
	Arguments        []string
	WorkingDirectory string
	Environment      []protocol.EnvironmentEntry
	Stdin            []byte
}

type Spec struct {
	Identity         protocol.Identity
	Command          Command
	Deadline         time.Duration
	TerminationGrace time.Duration
}

// Owner names an explicit helper binary so suites can build it once and share
// it without relying on a fixed installation path.
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
	info, err := os.Stat(helperPath)
	if err != nil {
		return nil, fmt.Errorf("inspect process-owner helper: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("process-owner helper must be a regular file")
	}
	return &Owner{helperPath: helperPath}, nil
}

// HelperExecutable exposes the already-validated suite artifact for an outer
// parent-death harness. Normal target execution must still enter through Start;
// the exceptional harness needs to launch a client which then owns its own tree.
func (owner *Owner) HelperExecutable() string {
	if owner == nil {
		return ""
	}
	return owner.helperPath
}

// BuildOwner creates a suite-scoped helper from the checked-out repository.
// The returned Owner removes only its private build directory when closed.
func BuildOwner(ctx context.Context, repositoryRoot string) (_ *Owner, resultErr error) {
	if !filepath.IsAbs(repositoryRoot) || filepath.Clean(repositoryRoot) != repositoryRoot {
		return nil, errors.New("repository root must be absolute and canonical")
	}
	buildDirectory, err := os.MkdirTemp("", "windshare-testprocessowner-*")
	if err != nil {
		return nil, fmt.Errorf("create process-owner build directory: %w", err)
	}
	keepDirectory := false
	defer func() {
		if !keepDirectory {
			resultErr = errors.Join(resultErr, removeOwnedDirectory(buildDirectory))
		}
	}()
	helperName := "testprocessowner"
	if runtime.GOOS == "windows" {
		helperName += ".exe"
	}
	helperPath := filepath.Join(buildDirectory, helperName)
	command := exec.CommandContext(ctx, goExecutable(), "build", "-trimpath", "-o", helperPath, helperCommandPackage)
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("build process-owner helper: %w: %s", err, boundedText(output))
	}
	owner, err := NewOwner(helperPath)
	if err != nil {
		return nil, err
	}
	if err := owner.SelfCheck(ctx); err != nil {
		return nil, err
	}
	owner.ownedDirectory = buildDirectory
	keepDirectory = true
	return owner, nil
}

func goExecutable() string {
	// Nested helper builds honor an explicitly selected Go executable, while
	// direct developer runs keep conventional PATH behavior.
	if executable := os.Getenv(goExecutableEnvironment); executable != "" {
		return executable
	}
	return "go"
}

func (owner *Owner) SelfCheck(ctx context.Context) error {
	command := exec.CommandContext(ctx, owner.helperPath, "self-check")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("run process-owner self-check: %w", err)
	}
	if string(output) != helperSelfCheck {
		return fmt.Errorf("process-owner self-check returned an unexpected contract: %q", boundedText(output))
	}
	return nil
}

func (owner *Owner) Start(ctx context.Context, spec Spec) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("owned process context is nil")
	}
	// The process session outlives Start. Freezing caller-owned buffers here keeps
	// later mutations from changing the authenticated request or streamed input.
	frozen := spec
	frozen.Command.Arguments = slices.Clone(spec.Command.Arguments)
	frozen.Command.Environment = slices.Clone(spec.Command.Environment)
	frozen.Command.Stdin = bytes.Clone(spec.Command.Stdin)
	request, err := requestFromSpec(frozen)
	if err != nil {
		return nil, err
	}
	owner.mu.Lock()
	if owner.closed {
		owner.mu.Unlock()
		return nil, errors.New("process owner is closed")
	}
	owner.active++
	owner.mu.Unlock()
	output := newProcessOutput()
	session, err := startPlatform(ctx, owner.helperPath, frozen, request, output)
	if err != nil {
		owner.releaseProcess()
		return nil, err
	}
	process := newProcessWithOutput(&request, request.Identity, session, owner.releaseProcess, output)
	go process.stopWhenDone(ctx)
	return process, nil
}

func (owner *Owner) Close() error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.closed {
		return owner.closeErr
	}
	if owner.active != 0 {
		return fmt.Errorf("cannot close process owner with %d active processes", owner.active)
	}
	owner.closed = true
	if owner.ownedDirectory == "" {
		return nil
	}
	owner.closeErr = removeOwnedDirectory(owner.ownedDirectory)
	return owner.closeErr
}

func (owner *Owner) releaseProcess() {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.active--
}

func requestFromSpec(spec Spec) (protocol.Request, error) {
	deadline, err := durationMilliseconds("deadline", spec.Deadline)
	if err != nil {
		return protocol.Request{}, err
	}
	grace, err := durationMilliseconds("termination grace", spec.TerminationGrace)
	if err != nil {
		return protocol.Request{}, err
	}
	var stdin *protocol.Stdin
	if len(spec.Command.Stdin) > 0 {
		stdin = &protocol.Stdin{ByteLength: int64(len(spec.Command.Stdin))}
	}
	request := protocol.NewRequest(spec.Identity, protocol.Command{
		Executable: spec.Command.Executable, Arguments: spec.Command.Arguments,
		WorkingDirectory: spec.Command.WorkingDirectory,
		Environment:      spec.Command.Environment,
		Stdin:            stdin,
	}, deadline, grace)
	if err := protocol.ValidateRequest(request); err != nil {
		return protocol.Request{}, fmt.Errorf("validate owned process: %w", err)
	}
	return request, nil
}

func durationMilliseconds(label string, duration time.Duration) (int64, error) {
	if duration <= 0 || duration%time.Millisecond != 0 {
		return 0, fmt.Errorf("%s must be a positive whole number of milliseconds", label)
	}
	return duration.Milliseconds(), nil
}

func boundedText(value []byte) string {
	text := strings.ToValidUTF8(string(value), "�")
	if len(text) > protocol.MaximumDiagnosticBytes {
		text = text[:protocol.MaximumDiagnosticBytes]
	}
	return text
}

func removeOwnedDirectory(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private process-owner build directory %q: %w", path, err)
	}
	return nil
}
