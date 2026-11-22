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
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/module"
)

type blobConsumer func(file committedFile, size int64, body io.Reader) error

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
			return nil, fmt.Errorf("git returned a malformed tree record: %q", record)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("git returned a malformed tree header: %q", header)
		}
		mode, objectType, objectID := fields[0], fields[1], fields[2]
		if objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf("committed module path is not a regular file: %s", gitPathBytes)
		}
		if !isLowerHexCommitSHA(objectID) {
			return nil, fmt.Errorf("committed module path has an invalid object ID: %s", gitPathBytes)
		}

		relativePath := string(gitPathBytes)
		if strings.Contains(relativePath, "\\") {
			return nil, fmt.Errorf("git returned a non-canonical path: %q", relativePath)
		}
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

func streamCommittedBlobs(repositoryRoot string, files []committedFile, consume blobConsumer) (streamErr error) {
	// One batched cat-file stream avoids per-file Git startup overhead on
	// Windows and writes archives directly from commit blobs without staging.
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
		if streamErr != nil {
			// A Git launcher may have handed the pipe to a child that is still
			// writing, so close the pipe before reaping the process.
			_ = output.Close()
			_ = command.Process.Kill()
		}
		waitErr := command.Wait()
		if streamErr == nil {
			streamErr = waitErr
		}
		if streamErr != nil {
			if message := strings.TrimSpace(stderr.String()); message != "" {
				streamErr = fmt.Errorf("%w: %s", streamErr, message)
			}
			streamErr = fmt.Errorf("git cat-file --batch: %w", streamErr)
		}
	}()

	responses := bufio.NewReader(output)
	for _, file := range files {
		if !isSafeModulePath(file.relativePath) {
			return fmt.Errorf("invalid committed module path: %q", file.relativePath)
		}
		if err := consumeCommittedBlob(responses, file, consume); err != nil {
			return fmt.Errorf("stream %s from Git blob %s: %w", file.relativePath, file.objectID, err)
		}
	}
	_, err = responses.ReadByte()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected output after the last Git blob")
	}
	return fmt.Errorf("finish Git blob responses: %w", err)
}

func consumeCommittedBlob(responses *bufio.Reader, file committedFile, consume blobConsumer) error {
	header, err := responses.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read blob header: %w", err)
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != file.objectID || fields[1] != "blob" {
		return fmt.Errorf("unexpected blob header %q", header)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("invalid blob size %q", fields[2])
	}
	// Git frames arbitrary bytes by length, so payload newlines belong to the
	// blob. Only the newline after exactly size bytes separates responses.
	limited := &io.LimitedReader{R: responses, N: size}
	if err := consume(file, size, limited); err != nil {
		return err
	}
	if limited.N != 0 {
		return fmt.Errorf("blob consumer left %d unread bytes", limited.N)
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
