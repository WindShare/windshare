package testrelaynative

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeRelayArtifactUsesCargoReportedPath(t *testing.T) {
	// Cargo configuration may relocate even a fresh cached artifact.
	executable := filepath.Join(t.TempDir(), "custom-target", binaryName)
	artifact, err := json.Marshal(map[string]any{
		"reason": "compiler-artifact", "target": map[string]string{"name": binaryName},
		"executable": executable, "fresh": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	output := "{\"reason\":\"compiler-artifact\",\"target\":{\"name\":\"dependency\"},\"executable\":null}\n" + string(artifact)
	got, err := nativeRelayArtifact(strings.NewReader(output))
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
		if _, err := nativeRelayArtifact(strings.NewReader(invalid)); err == nil {
			t.Fatalf("accepted missing or invalid Cargo executable: %s", invalid)
		}
	}
}

func TestExecutableCopyIsIndependentOfCargoCache(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "cargo-executable")
	destination := filepath.Join(directory, "suite-executable")
	if err := os.WriteFile(source, []byte("first build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("later build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutable(source, destination); err == nil {
		t.Fatal("overwrote an existing suite executable")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "first build" {
		t.Fatalf("suite executable = %q, %v", got, err)
	}
	if err := copyExecutable(filepath.Join(directory, "missing"), destination); err == nil {
		t.Fatal("accepted a missing Cargo executable")
	}
}
