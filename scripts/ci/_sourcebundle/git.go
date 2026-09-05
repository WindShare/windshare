package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/module"
)

func committedModuleFiles(repositoryRoot, commitSHA string) ([]committedFile, error) {
	objectType, err := gitOutput(repositoryRoot, "cat-file", "-t", commitSHA)
	if err != nil {
		return nil, fmt.Errorf("inspect release commit: %w", err)
	}
	if strings.TrimSpace(string(objectType)) != "commit" {
		return nil, errors.New("release SHA must directly identify a commit object")
	}

	output, err := gitOutput(
		repositoryRoot,
		"ls-tree", "-r", "-z", "--full-tree", commitSHA,
	)
	if err != nil {
		return nil, fmt.Errorf("enumerate committed module files: %w", err)
	}
	return parseCommittedModuleFiles(output)
}

func gitOutput(repositoryRoot string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = isolatedGitEnvironment()
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil, fmt.Errorf(
			"git %s: %s",
			strings.Join(arguments, " "),
			strings.TrimSpace(string(exitError.Stderr)),
		)
	}
	return nil, fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
}

func isolatedGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+4)
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		environment = append(environment, variable)
	}
	// The publication set must come from the named repository, not caller
	// redirects or machine-global ignore configuration.
	return append(
		environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func parseCommittedModuleFiles(output []byte) ([]committedFile, error) {
	seen := make(map[string]struct{})
	files := make([]committedFile, 0)
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, gitPathBytes, found := bytes.Cut(record, []byte{'\t'})
		if !found {
			return nil, fmt.Errorf("Git returned a malformed tree record: %q", record)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("Git returned a malformed tree header: %q", header)
		}
		mode, objectType, objectID := fields[0], fields[1], fields[2]
		if objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf("committed module path is not a regular file: %s", gitPathBytes)
		}
		if !isLowerHexCommitSHA(objectID) {
			return nil, fmt.Errorf("committed module path has an invalid object ID: %s", gitPathBytes)
		}

		fullGitPath := string(gitPathBytes)
		if strings.Contains(fullGitPath, "\\") {
			return nil, fmt.Errorf("Git returned a non-canonical path: %q", fullGitPath)
		}
		relativePath := fullGitPath
		if !isSafeModulePath(relativePath) {
			return nil, fmt.Errorf("invalid root module path: %q", relativePath)
		}
		if _, duplicate := seen[relativePath]; duplicate {
			return nil, fmt.Errorf("duplicate root module path: %s", relativePath)
		}
		seen[relativePath] = struct{}{}
		files = append(files, committedFile{relativePath: relativePath, objectID: objectID, executable: mode == "100755"})
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].relativePath < files[right].relativePath
	})
	return files, nil
}

func committedFilePaths(files []committedFile) []string {
	paths := make([]string, len(files))
	for index, file := range files {
		paths[index] = file.relativePath
	}
	return paths
}

func isSafeModulePath(filePath string) bool {
	if filePath == "" || filePath == "." || path.IsAbs(filePath) || strings.Contains(filePath, "\\") {
		return false
	}
	cleaned := path.Clean(filePath)
	if cleaned != filePath || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return module.CheckFilePath(filePath) == nil
}

func stageFiles(repositoryRoot, stageDirectory string, files []committedFile) error {
	for _, file := range files {
		relativePath := file.relativePath
		if !isSafeModulePath(relativePath) {
			return fmt.Errorf("invalid staged module path: %q", relativePath)
		}
		destinationPath := filepath.Join(stageDirectory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return fmt.Errorf("create staging parent for %s: %w", relativePath, err)
		}
		if err := copyCommittedBlob(repositoryRoot, file.objectID, destinationPath); err != nil {
			return err
		}
		if file.executable {
			if err := os.Chmod(destinationPath, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyCommittedBlob(repositoryRoot, objectID, destinationPath string) error {
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", destinationPath, err)
	}
	command := exec.Command("git", "-C", repositoryRoot, "cat-file", "blob", objectID)
	command.Env = isolatedGitEnvironment()
	command.Stdout = destination
	var stderr bytes.Buffer
	command.Stderr = &stderr
	copyErr := command.Run()
	closeErr := destination.Close()
	if copyErr != nil {
		removeErr := os.Remove(destinationPath)
		return errors.Join(
			fmt.Errorf("read committed blob %s: %s: %w", objectID, strings.TrimSpace(stderr.String()), copyErr),
			removeErr,
		)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", destinationPath, closeErr)
	}
	return nil
}
