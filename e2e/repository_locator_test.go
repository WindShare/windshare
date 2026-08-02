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
	e2eCoreModulePath            = "github.com/windshare/windshare/core"
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
		panic("e2e: locate repository workspace: " + e2eRepositoryRootErr.Error())
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
	if err := requireModuleIdentity(
		locator.readFile,
		filepath.Join(root, "core", "go.mod"),
		e2eCoreModulePath,
	); err != nil {
		return "", err
	}
	workPath := filepath.Join(root, "go.work")
	workBytes, err := locator.readFile(workPath)
	if err != nil {
		return "", fmt.Errorf("read workspace identity %s: %w", workPath, err)
	}
	work, err := modfile.ParseWork(workPath, workBytes, nil)
	if err != nil {
		return "", fmt.Errorf("parse workspace identity %s: %w", workPath, err)
	}
	uses := make(map[string]bool, len(work.Use))
	for _, use := range work.Use {
		path := use.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		uses[filepath.Clean(path)] = true
	}
	if !uses[root] || !uses[filepath.Join(root, "core")] {
		return "", errors.New("go.work must select the root and core module directories")
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
	if actual := modfile.ModulePath(encoded); actual != expectedModulePath {
		return fmt.Errorf("module identity %s = %q, want %q", path, actual, expectedModulePath)
	}
	return nil
}

func TestE2ERepositoryLocatorUsesBoundedWorkspaceIdentity(t *testing.T) {
	root := t.TempDir()
	packageDirectory := filepath.Join(root, "e2e")
	if err := os.MkdirAll(filepath.Join(root, "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(packageDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryLocatorFixture(t, filepath.Join(root, "go.mod"), "module "+e2eRootModulePath+"\n")
	writeRepositoryLocatorFixture(
		t,
		filepath.Join(root, "core", "go.mod"),
		"module "+e2eCoreModulePath+"\n",
	)
	writeRepositoryLocatorFixture(t, filepath.Join(root, "go.work"), "go 1.26.5\n\nuse (\n\t.\n\t./core\n)\n")

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
	build.Env = replaceEnvironment(os.Environ(), "GOFLAGS", "-trimpath")
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
