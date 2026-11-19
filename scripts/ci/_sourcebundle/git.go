package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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

func gitCommand(repositoryRoot string, arguments ...string) *exec.Cmd {
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = isolatedGitEnvironment()
	return command
}

func gitOutput(repositoryRoot string, arguments ...string) ([]byte, error) {
	output, err := gitCommand(repositoryRoot, arguments...).Output()
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

func stageFiles(repositoryRoot, stageDirectory string, files []committedFile) (stageErr error) {
	// Git startup dominates source staging on Windows. Batch requests amortize
	// that cost while still reading the exact object IDs from the commit tree.
	command := gitCommand(repositoryRoot, "cat-file", "--batch", "--buffer")
	var requests strings.Builder
	for _, file := range files {
		requests.WriteString(file.objectID)
		requests.WriteByte('\n')
	}
	command.Stdin = strings.NewReader(requests.String())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Git blob response pipe: %w", err)
	}
	defer output.Close()
	if err := command.Start(); err != nil {
		return fmt.Errorf("start git cat-file --batch: %w", err)
	}
	defer func() {
		if stageErr != nil {
			// A Git launcher may have handed the pipe to a child that is still
			// writing, so close the pipe before reaping the process.
			_ = output.Close()
			_ = command.Process.Kill()
		}
		waitErr := command.Wait()
		if stageErr == nil {
			stageErr = waitErr
		}
		if stageErr != nil {
			if message := strings.TrimSpace(stderr.String()); message != "" {
				stageErr = fmt.Errorf("%w: %s", stageErr, message)
			}
			stageErr = fmt.Errorf("git cat-file --batch: %w", stageErr)
		}
	}()

	responses := bufio.NewReader(output)
	if err := stageCommittedFiles(stageDirectory, files, responses); err != nil {
		return err
	}
	_, err = responses.ReadByte()
	switch err {
	case io.EOF:
		return nil
	case nil:
		return errors.New("unexpected output after the last Git blob")
	default:
		return fmt.Errorf("finish Git blob responses: %w", err)
	}
}

func stageCommittedFiles(stageDirectory string, files []committedFile, responses *bufio.Reader) error {
	for _, file := range files {
		relativePath := file.relativePath
		if !isSafeModulePath(relativePath) {
			return fmt.Errorf("invalid staged module path: %q", relativePath)
		}
		destinationPath := filepath.Join(stageDirectory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return fmt.Errorf("create staging parent for %s: %w", relativePath, err)
		}
		if err := stageCommittedBlob(responses, file.objectID, destinationPath); err != nil {
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

func stageCommittedBlob(responses *bufio.Reader, objectID, destinationPath string) error {
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", destinationPath, err)
	}
	copyErr := copyCommittedBlob(responses, objectID, destination)
	if err := errors.Join(copyErr, destination.Close()); err != nil {
		return errors.Join(
			fmt.Errorf("stage %s from Git blob %s: %w", destinationPath, objectID, err),
			os.Remove(destinationPath),
		)
	}
	return nil
}

func copyCommittedBlob(responses *bufio.Reader, objectID string, destination io.Writer) error {
	header, err := responses.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read blob header: %w", err)
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != objectID || fields[1] != "blob" {
		return fmt.Errorf("unexpected blob header %q", header)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("invalid blob size %q", fields[2])
	}
	// Git frames arbitrary bytes by length, so payload newlines belong to the
	// blob. Only the newline after exactly size bytes separates responses.
	if _, err := io.CopyN(destination, responses, size); err != nil {
		return fmt.Errorf("copy blob bytes: %w", err)
	}
	delimiter, err := responses.ReadByte()
	if err != nil {
		return fmt.Errorf("read blob delimiter: %w", err)
	}
	if delimiter != '\n' {
		return fmt.Errorf("invalid blob delimiter %q", delimiter)
	}
	return nil
}
