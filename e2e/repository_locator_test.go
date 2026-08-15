package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/modfile"
)

const (
	e2eRootModulePath            = "github.com/windshare/windshare"
	e2eRelocationHelperEnv       = "WINDSHARE_E2E_RELOCATION_HELPER"
	e2eRelocationExpectedRootEnv = "WINDSHARE_E2E_RELOCATION_EXPECTED_ROOT"
)

type e2eRepositoryLocator struct {
	workingDirectory func() (string, error)
	readFile         func(string) ([]byte, error)
}

var (
	e2eRepositoryRootOnce sync.Once
	e2eRepositoryRoot     string
	e2eRepositoryRootErr  error
)

func repoRoot() string {
	e2eRepositoryRootOnce.Do(func() {
		e2eRepositoryRoot, e2eRepositoryRootErr = locateE2ERepositoryRoot(e2eRepositoryLocator{
			workingDirectory: os.Getwd,
			readFile:         os.ReadFile,
		})
	})
	if e2eRepositoryRootErr != nil {
		panic("e2e: locate repository root: " + e2eRepositoryRootErr.Error())
	}
	return e2eRepositoryRoot
}

func locateE2ERepositoryRoot(locator e2eRepositoryLocator) (string, error) {
	if locator.workingDirectory == nil || locator.readFile == nil {
		return "", errors.New("repository locator dependencies are required")
	}
	workingDirectory, err := locator.workingDirectory()
	if err != nil {
		return "", fmt.Errorf("read package working directory: %w", err)
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return "", fmt.Errorf("canonicalize package working directory: %w", err)
	}
	workingDirectory = filepath.Clean(workingDirectory)
	if filepath.Base(workingDirectory) != "e2e" {
		return "", fmt.Errorf("package working directory %q is not the e2e directory", workingDirectory)
	}
	root := filepath.Dir(workingDirectory)
	if err := requireModuleIdentity(locator.readFile, filepath.Join(root, "go.mod"), e2eRootModulePath); err != nil {
		return "", err
	}
	return root, nil
}

func requireModuleIdentity(
	readFile func(string) ([]byte, error),
	path string,
	expectedModulePath string,
) error {
	encoded, err := readFile(path)
	if err != nil {
		return fmt.Errorf("read module identity %s: %w", path, err)
	}
	parsed, err := modfile.Parse(path, encoded, nil)
	if err != nil {
		return fmt.Errorf("parse module identity %s: %w", path, err)
	}
	if parsed.Module == nil {
		return fmt.Errorf("module identity %s has no module directive", path)
	}
	if actual := parsed.Module.Mod.Path; actual != expectedModulePath {
		return fmt.Errorf("module identity %s = %q, want %q", path, actual, expectedModulePath)
	}
	return nil
}

func TestE2ERepositoryLocatorUsesBoundedModuleIdentity(t *testing.T) {
	root := t.TempDir()
	packageDirectory := filepath.Join(root, "e2e")
	if err := os.Mkdir(packageDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryLocatorFixture(t, filepath.Join(root, "go.mod"), "module "+e2eRootModulePath+"\n")

	located, err := locateE2ERepositoryRoot(e2eRepositoryLocator{
		workingDirectory: func() (string, error) { return packageDirectory, nil },
		readFile:         os.ReadFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if located != root {
		t.Fatalf("located root = %q, want %q", located, root)
	}
}

func TestE2ERepositoryLocatorRejectsInvalidRootModuleIdentity(t *testing.T) {
	tests := []struct {
		name      string
		contents  string
		writeFile bool
		wantError string
	}{
		{name: "missing", wantError: "read module identity"},
		{name: "malformed", contents: "module\n", writeFile: true, wantError: "parse module identity"},
		{name: "wrong path", contents: "module example.com/not-windshare\n", writeFile: true, wantError: "want \"" + e2eRootModulePath + "\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			packageDirectory := filepath.Join(root, "e2e")
			if err := os.Mkdir(packageDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			if test.writeFile {
				writeRepositoryLocatorFixture(t, filepath.Join(root, "go.mod"), test.contents)
			}
			_, err := locateE2ERepositoryRoot(e2eRepositoryLocator{
				workingDirectory: func() (string, error) { return packageDirectory, nil },
				readFile:         os.ReadFile,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestE2ERepositoryRootSupportsRelocatedTrimpathBinary(t *testing.T) {
	if os.Getenv(e2eRelocationHelperEnv) == "1" {
		expected := os.Getenv(e2eRelocationExpectedRootEnv)
		if actual := repoRoot(); actual != expected {
			t.Fatalf("relocated repository root = %q, want %q", actual, expected)
		}
		return
	}

	root := repoRoot()
	buildDirectory := t.TempDir()
	relocatedDirectory := t.TempDir()
	builtBinary := filepath.Join(buildDirectory, exeName("e2e-relocation-contract"))
	relocatedBinary := filepath.Join(relocatedDirectory, exeName("e2e-relocation-contract"))
	build := exec.Command(e2eGoExecutable(), "test", "-c", "-o", builtBinary, "./e2e")
	build.Dir = root
	build.Env = e2eChildEnvironment(replaceEnvironment(os.Environ(), "GOFLAGS", "-trimpath"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trimpath relocation contract: %v\n%s", err, output)
	}
	if err := os.Rename(builtBinary, relocatedBinary); err != nil {
		t.Fatal(err)
	}

	run := exec.Command(relocatedBinary, "-test.run=^TestE2ERepositoryRootSupportsRelocatedTrimpathBinary$")
	run.Dir = filepath.Join(root, "e2e")
	run.Env = replaceEnvironment(
		replaceEnvironment(os.Environ(), e2eRelocationHelperEnv, "1"),
		e2eRelocationExpectedRootEnv,
		root,
	)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run relocated trimpath contract: %v\n%s", err, output)
	}
}

func TestE2EChildEnvironmentDisablesWorkspaceResolution(t *testing.T) {
	environment := e2eChildEnvironment([]string{
		"PATH=value", "GOWORK=auto", "gowork=unexpected", "OTHER=kept",
	})
	want := []string{"PATH=value", "OTHER=kept", "GOWORK=off"}
	if strings.Join(environment, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("environment = %q, want %q", environment, want)
	}
}

func writeRepositoryLocatorFixture(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func replaceEnvironment(environment []string, name string, value string) []string {
	replaced := make([]string, 0, len(environment)+1)
	prefix := name + "="
	for _, entry := range environment {
		if strings.EqualFold(strings.SplitN(entry, "=", 2)[0]+"=", prefix) {
			continue
		}
		replaced = append(replaced, entry)
	}
	return append(replaced, prefix+value)
}
