package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestProtocolsCannotReenterProductionObservations(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{
		"engine/internal/receive/events.go",
		"cmd/wind/internal/cli/process_trace.go",
		"engine/internal/receive/events_test.go",
		"cmd/testprocessowner/main.go",
	} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		source := "package example\nimport _ \"" + module + "/internal/testrun\"\n"
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := sourceFindings(root)
	if err != nil || len(findings) != 2 {
		t.Fatalf("production dependency gate: %v %v", findings, err)
	}
	for _, finding := range findings {
		if strings.Contains(finding, "_test.go") || strings.Contains(finding, "cmd/testprocessowner") {
			t.Fatalf("test owner was treated as production: %s", finding)
		}
	}
	testProtocol := types.NewPackage(module+"/internal/testrun", "testrun")
	milestone := types.NewNamed(types.NewTypeName(token.NoPos, testProtocol, "Milestone", nil), types.Typ[types.String], nil)
	if findings := typeFindings("Observation", milestone); len(findings) != 1 {
		t.Fatalf("test protocol escaped through public value type: %v", findings)
	}
}
