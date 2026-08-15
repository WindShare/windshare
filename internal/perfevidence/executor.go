package perfevidence

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	localToolchainEnvironment = "GOTOOLCHAIN"
	workspaceEnvironment      = "GOWORK"
)

type Command struct {
	Executable string
	Arguments  []string
	Directory  string
}

type CommandResult struct {
	Output     []byte
	ProcessID  int
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
}

type ExecRunner struct {
	Environment []string
	Now         func() time.Time
}

func (runner ExecRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	result := CommandResult{ExitCode: -1}
	if ctx == nil {
		return result, errors.New("command context is nil")
	}
	now := runner.Now
	if now == nil {
		now = time.Now
	}
	child := exec.CommandContext(ctx, command.Executable, command.Arguments...)
	child.Dir = command.Directory
	environment := runner.Environment
	if environment == nil {
		environment = os.Environ()
	}
	child.Env = commandEnvironment(environment)
	var output bytes.Buffer
	child.Stdout = &output
	child.Stderr = &output
	result.StartedAt = now().UTC()
	err := child.Run()
	result.FinishedAt = now().UTC()
	result.Output = append([]byte(nil), output.Bytes()...)
	if child.Process != nil {
		result.ProcessID = child.Process.Pid
	}
	if child.ProcessState != nil {
		result.ExitCode = child.ProcessState.ExitCode()
	}
	if cause := context.Cause(ctx); cause != nil {
		err = errors.Join(err, cause)
	}
	return result, err
}

func commandEnvironment(environment []string) []string {
	environment = replaceEnvironment(environment, localToolchainEnvironment, "local")
	return replaceEnvironment(environment, workspaceEnvironment, "off")
}

func replaceEnvironment(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	replaced := false
	for _, assignment := range environment {
		variable, _, found := strings.Cut(assignment, "=")
		if !found || !strings.EqualFold(variable, name) {
			result = append(result, assignment)
			continue
		}
		if !replaced {
			result = append(result, name+"="+value)
			replaced = true
		}
	}
	if !replaced {
		result = append(result, name+"="+value)
	}
	return result
}
