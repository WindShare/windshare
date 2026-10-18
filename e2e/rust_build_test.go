package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const e2eNativeRelayBinary = "relay-native"

func buildE2ENativeRelay(destination string) error {
	// Cargo owns its persistent target directory, including caller overrides.
	// Running a suite-local copy avoids locking that cache on Windows.
	command := exec.Command("rustup", "run", "stable", "cargo", "build", "--locked",
		"-p", "windshare-relay-native", "--bin", e2eNativeRelayBinary,
		"--message-format=json-render-diagnostics")
	command.Dir = filepath.Join(repoRoot(), "relay-rust")
	command.Env = e2eChildEnvironment(os.Environ())
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("build Rust native relay: %w\n%s\n%s", err, &stderr, &stdout)
	}
	artifact, err := e2eNativeRelayArtifact(&stdout)
	if err != nil {
		return fmt.Errorf("locate Rust native relay: %w\n%s", err, &stderr)
	}
	return copyE2EExecutable(artifact, destination)
}

func e2eNativeRelayArtifact(output io.Reader) (string, error) {
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
		if message.Reason != "compiler-artifact" || message.Target.Name != e2eNativeRelayBinary || message.Executable == "" {
			continue
		}
		if !filepath.IsAbs(message.Executable) {
			return "", fmt.Errorf("Cargo executable path is not absolute: %q", message.Executable)
		}
		return message.Executable, nil
	}
}

func copyE2EExecutable(source, destination string) error {
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
	return wrapError("copy Cargo executable", errors.Join(copyErr, output.Close()))
}

func TestE2ENativeRelayArtifactUsesCargoReportedPath(t *testing.T) {
	// The location can change with Cargo configuration or CARGO_TARGET_DIR, and
	// fresh cached artifacts must work without a rebuild.
	executable := filepath.Join(t.TempDir(), "custom-target", exeName(e2eNativeRelayBinary))
	artifact, err := json.Marshal(map[string]any{
		"reason": "compiler-artifact", "target": map[string]string{"name": e2eNativeRelayBinary},
		"executable": executable, "fresh": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	output := "{\"reason\":\"compiler-artifact\",\"target\":{\"name\":\"dependency\"},\"executable\":null}\n" + string(artifact)
	got, err := e2eNativeRelayArtifact(strings.NewReader(output))
	if err != nil || got != executable {
		t.Fatalf("Cargo artifact = %q, %v; want %q", got, err, executable)
	}
	for _, invalid := range []string{
		"",
		"not JSON",
		`{"reason":"build-finished","success":true}`,
		`{"reason":"compiler-artifact","target":{"name":"relay-native"},"executable":null}`,
		`{"reason":"compiler-artifact","target":{"name":"relay-native"},"executable":"relative/relay-native"}`,
	} {
		if _, err := e2eNativeRelayArtifact(strings.NewReader(invalid)); err == nil {
			t.Fatalf("accepted missing or invalid Cargo executable: %s", invalid)
		}
	}
}
