package main

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
	"strings"
	"time"
)

const nativeRelayPackage = "windshare-relay-native"
const nativeRelayBinary = "relay-native"
const nativeRelayProbeTimeout = 5 * time.Second

func nativeReleaseTarget(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "x86_64-unknown-linux-musl", nil
	case "windows/amd64":
		return "x86_64-pc-windows-msvc", nil
	default:
		return "", fmt.Errorf("unsupported native relay release target %s/%s", goos, goarch)
	}
}

func buildNativeRelay(source, destination string) error {
	target, err := nativeReleaseTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	workspace := filepath.Join(source, "relay-rust")
	command := exec.Command("rustup", "run", "stable", "cargo", "build",
		"--locked", "--release", "--package", nativeRelayPackage, "--bin", nativeRelayBinary,
		"--target", target, "--target-dir", filepath.Join(workspace, ".target"),
		"--message-format=json-render-diagnostics")
	command.Dir = workspace
	command.Env = nativeReleaseEnvironment(os.Environ())
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("build native relay for %s: %w", target, err)
	}
	executable, err := nativeReleaseArtifact(&output)
	if err != nil {
		return err
	}
	if err := copyFile(executable, destination); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o755); err != nil {
		return err
	}
	// Probe the shipped file, so a wrong-target executable cannot survive until
	// a user tries to start the installed server.
	ctx, cancel := context.WithTimeout(context.Background(), nativeRelayProbeTimeout)
	defer cancel()
	if output, err := exec.CommandContext(ctx, destination, "--help").CombinedOutput(); err != nil {
		return fmt.Errorf("probe release relay: %s: %w", output, err)
	}
	return nil
}

func nativeReleaseEnvironment(inherited []string) []string {
	environment := make([]string, 0, len(inherited)+1)
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "CARGO_BUILD_TARGET", "CARGO_TARGET_DIR", "CARGO_ENCODED_RUSTFLAGS", "RUSTFLAGS":
			continue
		}
		environment = append(environment, item)
	}
	// Release ZIPs must run without a separately installed C runtime. The
	// explicit Cargo target also keeps this flag off host build scripts.
	return append(environment, "RUSTFLAGS=-C target-feature=+crt-static")
}

func nativeReleaseArtifact(output io.Reader) (string, error) {
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
				return "", errors.New("cargo did not produce the release relay executable")
			}
			return "", fmt.Errorf("decode release relay artifact: %w", err)
		}
		if message.Reason != "compiler-artifact" || message.Target.Name != nativeRelayBinary || message.Executable == "" {
			continue
		}
		if !filepath.IsAbs(message.Executable) {
			return "", fmt.Errorf("release relay executable is not absolute: %q", message.Executable)
		}
		return message.Executable, nil
	}
}
