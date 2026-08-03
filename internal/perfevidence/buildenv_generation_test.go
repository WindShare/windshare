package perfevidence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testToolchainGenerationA = "generation-a"
	testToolchainGenerationB = "generation-b"
)

func TestControlledGoEnvironmentRetainsOneToolchainGenerationAcrossTerminals(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "sdk")
	goExecutable := filepath.Join(sourceRoot, "bin", "go")
	if runtime.GOOS == "windows" {
		goExecutable += ".exe"
	}
	goToolDir := filepath.Join(sourceRoot, "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH)
	includeRoot := filepath.Join(sourceRoot, filepath.FromSlash(goAssemblyIncludeRelativePath))
	for _, directory := range []string{filepath.Dir(goExecutable), goToolDir, includeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	helpTool := filepath.Join(goToolDir, "compile")
	for path, content := range map[string]string{
		goExecutable:                             testToolchainGenerationA,
		helpTool:                                 "compiler-a",
		filepath.Join(includeRoot, "textflag.h"): "header-a",
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repositoryRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repositoryRoot, "go.work"), []byte("go 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(goExecutableEnvironment, goExecutable)
	runner := &generationSwapRunner{
		sourceExecutable: goExecutable,
		sourceHelper:     helpTool,
	}
	environment, err := prepareControlledGoEnvironment(
		context.Background(), runner, repositoryRoot, t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeConsumptionAuthority(environment.Authority); err != nil {
			t.Errorf("close toolchain authority: %v", err)
		}
	})
	if got := runner.generations; len(got) != 2 || got[0] != testToolchainGenerationA || got[1] != testToolchainGenerationA {
		t.Fatalf("metadata terminals observed mixed Go generations: %v", got)
	}
	if got := strings.TrimSpace(environment.Toolchain.Version); got != "go version "+testToolchainGenerationA {
		t.Fatalf("recorded Go version = %q", got)
	}
	if got := environment.Toolchain.GoVersion; got != "go1."+testToolchainGenerationA {
		t.Fatalf("recorded GOVERSION = %q", got)
	}
	materializedExecutable, err := os.ReadFile(environment.GoExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(materializedExecutable) != testToolchainGenerationA {
		t.Fatalf("retained executable generation = %q", materializedExecutable)
	}
	materializedHelper, err := os.ReadFile(filepath.Join(environment.ToolchainLocations.GoToolDir, "compile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(materializedHelper) != "compiler-a" {
		t.Fatalf("retained helper generation = %q", materializedHelper)
	}
	sourceGeneration, err := os.ReadFile(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceGeneration) != testToolchainGenerationB {
		t.Fatalf("hostile source transition did not run: %q", sourceGeneration)
	}
}

type generationSwapRunner struct {
	sourceExecutable string
	sourceHelper     string
	generations      []string
}

func (runner *generationSwapRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	generationBytes, err := os.ReadFile(command.Executable)
	if err != nil {
		return CommandResult{ExitCode: -1}, err
	}
	generation := string(generationBytes)
	runner.generations = append(runner.generations, generation)
	var output []byte
	switch {
	case len(command.Arguments) == 1 && command.Arguments[0] == "version":
		output = []byte("go version " + generation + "\n")
		// The alias/source SDK changes after the first observable terminal. Later
		// identity commands must remain bound to the already-retained generation.
		if err := os.WriteFile(runner.sourceExecutable, []byte(testToolchainGenerationB), 0o700); err != nil {
			return CommandResult{ExitCode: -1}, fmt.Errorf("replace source Go executable: %w", err)
		}
		if err := os.WriteFile(runner.sourceHelper, []byte("compiler-b"), 0o700); err != nil {
			return CommandResult{ExitCode: -1}, fmt.Errorf("replace source Go helper: %w", err)
		}
	case len(command.Arguments) == 2 && command.Arguments[0] == "env" && command.Arguments[1] == "-json":
		goRoot := testProcessEnvironmentValue(command.Environment, "GOROOT")
		encoded, marshalErr := json.Marshal(map[string]string{
			"GOROOT":    goRoot,
			"GOTOOLDIR": filepath.Join(goRoot, "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH),
			"GOVERSION": "go1." + generation,
		})
		if marshalErr != nil {
			return CommandResult{ExitCode: -1}, marshalErr
		}
		output = encoded
	default:
		return CommandResult{ExitCode: -1}, fmt.Errorf("unexpected fake Go command: %v", command.Arguments)
	}
	return CommandResult{Stdout: output, Output: output, ExitCode: 0}, nil
}

func testProcessEnvironmentValue(environment []string, name string) string {
	for _, entry := range environment {
		entryName, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(entryName, name) {
			return value
		}
	}
	return ""
}
