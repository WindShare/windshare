package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/internal/processowner"
)

func TestRunSelfCheckAndRejectsUnknownCommands(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{commandSelfCheck}, bytes.NewReader(nil), &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != selfCheckOutput {
		t.Fatalf("self-check output = %q", output.String())
	}
	for _, arguments := range [][]string{nil, {"unknown"}} {
		if err := run(arguments, bytes.NewReader(nil), &output); err == nil {
			t.Fatalf("arguments %v were accepted", arguments)
		}
	}
}

func TestRunRejectsInvalidSupervisionConfig(t *testing.T) {
	config := processowner.Config{
		Executable: filepath.Join(t.TempDir(), "target"), Arguments: []string{},
		WorkingDirectory: t.TempDir(), Environment: []string{},
		DeadlineMilliseconds: 1000, TerminationGraceMilliseconds: 100,
	}
	encoded, err := processowner.EncodeConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{commandSupervise}, bytes.NewReader(encoded), bytes.NewBuffer(nil)); err == nil {
		t.Fatal("missing platform endpoints were accepted")
	}
}

func TestMainSelfCheckReturnsWithoutExit(t *testing.T) {
	original := os.Args
	os.Args = []string{"testprocessowner", commandSelfCheck}
	t.Cleanup(func() { os.Args = original })
	main()
}
