// Package testrelaynative hosts the native Rust relay for Go correctness suites.
package testrelaynative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/windshare/windshare/internal/testprocess"
)

const binaryName = "relay-native"

// Build prepares a suite-local executable. Callers own the directory and invoke
// it once per suite, before starting any scenario deadline.
func Build(ctx context.Context, repositoryRoot, directory string) (string, error) {
	// Cargo owns its persistent cache, including CARGO_TARGET_DIR overrides.
	// A separate executable lets concurrent Windows suites rebuild that cache.
	command := exec.CommandContext(ctx, "rustup", "run", "stable", "cargo", "build", "--locked",
		"-p", "windshare-relay-native", "--bin", binaryName,
		"--message-format=json-render-diagnostics")
	command.Dir = filepath.Join(repositoryRoot, "relay-rust")
	environment, err := testprocess.InheritEnvironment(map[string]string{"GOWORK": "off"})
	if err != nil {
		return "", err
	}
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("build Rust native relay: %w\n%s\n%s", err, &stderr, &stdout)
	}
	artifact, err := nativeRelayArtifact(&stdout)
	if err != nil {
		return "", fmt.Errorf("locate Rust native relay: %w\n%s", err, &stderr)
	}
	name := binaryName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	destination := filepath.Join(directory, name)
	if err := copyExecutable(artifact, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func nativeRelayArtifact(output io.Reader) (string, error) {
	decoder := json.NewDecoder(output)
	for {
		var message struct {
			Reason string `json:"reason"`
			Target struct {
				Name string `json:"name"`
			} `json:"target"`
			Executable string `json:"executable"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return "", errors.New("Cargo did not report the native relay executable")
			}
			return "", fmt.Errorf("decode Cargo artifact: %w", err)
		}
		if message.Reason != "compiler-artifact" || message.Target.Name != binaryName || message.Executable == "" {
			continue
		}
		if !filepath.IsAbs(message.Executable) {
			return "", fmt.Errorf("Cargo executable path is not absolute: %q", message.Executable)
		}
		return message.Executable, nil
	}
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open Cargo executable: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create suite executable: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	if err := errors.Join(copyErr, output.Close()); err != nil {
		return fmt.Errorf("copy Cargo executable: %w", err)
	}
	return nil
}
