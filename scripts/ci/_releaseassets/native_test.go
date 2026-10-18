package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeReleaseSelectsStandaloneTargets(t *testing.T) {
	for _, platform := range []struct{ os, arch, target string }{
		{"linux", "amd64", "x86_64-unknown-linux-musl"},
		{"windows", "amd64", "x86_64-pc-windows-msvc"},
	} {
		target, err := nativeReleaseTarget(platform.os, platform.arch)
		if err != nil || target != platform.target {
			t.Fatalf("target for %s/%s = %q, %v", platform.os, platform.arch, target, err)
		}
	}
	if _, err := nativeReleaseTarget("linux", "unsupported"); err == nil {
		t.Fatal("unsupported distribution target accepted")
	}
	environment := nativeReleaseEnvironment([]string{
		"PATH=tools", "CARGO_HOME=registry", "RUSTFLAGS=ambient",
		"CARGO_ENCODED_RUSTFLAGS=ambient", "CARGO_BUILD_TARGET=ambient", "CARGO_TARGET_DIR=ambient",
	})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "ambient") || !strings.Contains(joined, "PATH=tools") ||
		!strings.Contains(joined, "CARGO_HOME=registry") || !strings.Contains(joined, "+crt-static") {
		t.Fatalf("release build inherited target or flag overrides: %q", environment)
	}
}

func TestNativeReleaseUsesCargoExecutableEvidence(t *testing.T) {
	executable := filepath.Join(t.TempDir(), nativeRelayBinary)
	message, err := json.Marshal(map[string]any{
		"reason": "compiler-artifact", "target": map[string]string{"name": nativeRelayBinary},
		"executable": executable,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := `{"reason":"build-script-executed"}
{"reason":"compiler-artifact","target":{"name":"dependency"},"executable":null}
` + string(message)
	got, err := nativeReleaseArtifact(strings.NewReader(stream))
	if err != nil || got != executable {
		t.Fatalf("artifact = %q, %v", got, err)
	}
	for _, invalid := range []string{
		"", "invalid JSON",
		`{"reason":"compiler-artifact","target":{"name":"relay-native"},"executable":"relative"}`,
		`{"reason":"build-finished","success":true}`,
	} {
		if _, err := nativeReleaseArtifact(strings.NewReader(invalid)); err == nil {
			t.Fatalf("accepted missing or invalid release artifact: %s", invalid)
		}
	}
}
